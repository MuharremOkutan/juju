// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package provisioner

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"
	"text/template"

	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/utils/arch"
	"github.com/juju/utils/exec"
	"github.com/juju/version"
	"gopkg.in/juju/names.v2"

	apiprovisioner "github.com/juju/juju/api/provisioner"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/container"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/network"
	"github.com/juju/juju/tools"
)

type APICalls interface {
	ContainerConfig() (params.ContainerConfig, error)
	PrepareContainerInterfaceInfo(names.MachineTag) ([]network.InterfaceInfo, error)
	GetContainerInterfaceInfo(names.MachineTag) ([]network.InterfaceInfo, error)
	ReleaseContainerAddresses(names.MachineTag) error
}

var _ APICalls = (*apiprovisioner.State)(nil)

type hostArchToolsFinder struct {
	f ToolsFinder
}

// FindTools is defined on the ToolsFinder interface.
func (h hostArchToolsFinder) FindTools(v version.Number, series, _ string) (tools.List, error) {
	// Override the arch constraint with the arch of the host.
	return h.f.FindTools(v, series, arch.HostArch())
}

// resolvConf is the full path to the resolv.conf file on the local
// system. Defined here so it can be overriden for testing.
var resolvConf = "/etc/resolv.conf"

// localDNSServers parses the /etc/resolv.conf file (if available) and
// extracts all nameservers addresses, and the default search domain
// and returns them.
func localDNSServers() ([]network.Address, string, error) {
	file, err := os.Open(resolvConf)
	if os.IsNotExist(err) {
		return nil, "", nil
	} else if err != nil {
		return nil, "", errors.Annotatef(err, "cannot open %q", resolvConf)
	}
	defer file.Close()

	var addresses []network.Address
	var searchDomain string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			// Skip comments.
			continue
		}
		if strings.HasPrefix(line, "nameserver") {
			address := strings.TrimPrefix(line, "nameserver")
			// Drop comments after the address, if any.
			if strings.Contains(address, "#") {
				address = address[:strings.Index(address, "#")]
			}
			address = strings.TrimSpace(address)
			addresses = append(addresses, network.NewAddress(address))
		}
		if strings.HasPrefix(line, "search") {
			searchDomain = strings.TrimPrefix(line, "search")
			// Drop comments after the domain, if any.
			if strings.Contains(searchDomain, "#") {
				searchDomain = searchDomain[:strings.Index(searchDomain, "#")]
			}
			searchDomain = strings.TrimSpace(searchDomain)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, "", errors.Annotatef(err, "cannot read DNS servers from %q", resolvConf)
	}
	return addresses, searchDomain, nil
}

// ipRouteAdd is the command template to add a static route for
// .ContainerIP using the .HostBridge device (usually lxcbr0 or virbr0).
var ipRouteAdd = mustParseTemplate("ipRouteAdd", `
ip route add {{.ContainerIP}} dev {{.HostBridge}}`[1:])

type IptablesRule struct {
	Table string
	Chain string
	Rule  string
}

var skipSNATRule = IptablesRule{
	// For EC2, to get internet access we need traffic to appear with
	// source address matching the container's host. For internal
	// traffic we want to keep the container IP because it is used
	// by some services. This rule sits above the SNAT rule, which
	// changes the source address of traffic to the container host IP
	// address, skipping this modification if the traffic destination
	// is inside the EC2 VPC.
	"nat",
	"POSTROUTING",
	"-d {{.SubnetCIDR}} -o {{.HostIF}} -j RETURN",
}

var iptablesRules = map[string]IptablesRule{
	// iptablesCheckSNAT is the command template to verify if a SNAT
	// rule already exists for the host NIC named .HostIF (usually
	// eth0) and source address .HostIP (usually eth0's address). We
	// need to check whether the rule exists because we only want to
	// add it once. Exit code 0 means the rule exists, 1 means it
	// doesn't.
	"iptablesSNAT": {
		"nat",
		"POSTROUTING",
		"-o {{.HostIF}} -j SNAT --to-source {{.HostIP}}",
	}, "iptablesForwardOut": {
		// Ensure that we have ACCEPT rules that apply to the containers that
		// we are creating so any DROP rules added by libvirt while setting
		// up virbr0 further down the chain don't disrupt wanted traffic.
		"filter",
		"FORWARD",
		"-d {{.ContainerCIDR}} -o {{.HostBridge}} -j ACCEPT",
	}, "iptablesForwardIn": {
		"filter",
		"FORWARD",
		"-s {{.ContainerCIDR}} -i {{.HostBridge}} -j ACCEPT",
	}}

// mustParseTemplate works like template.Parse, but panics on error.
func mustParseTemplate(name, source string) *template.Template {
	templ, err := template.New(name).Parse(source)
	if err != nil {
		panic(err.Error())
	}
	return templ
}

// mustExecTemplate works like template.Parse followed by template.Execute,
// but panics on error.
func mustExecTemplate(name, tmpl string, data interface{}) string {
	t := mustParseTemplate(name, tmpl)
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		panic(err.Error())
	}
	return buf.String()
}

// runTemplateCommand executes the given template with the given data,
// which generates a command to execute. If exitNonZeroOK is true, no
// error is returned if the exit code is not 0, otherwise an error is
// returned.
func runTemplateCommand(t *template.Template, exitNonZeroOK bool, data interface{}) (
	exitCode int, err error,
) {
	// Clone the template to ensure the original won't be changed.
	cloned, err := t.Clone()
	if err != nil {
		return -1, errors.Annotatef(err, "cannot clone command template %q", t.Name())
	}
	var buf bytes.Buffer
	if err := cloned.Execute(&buf, data); err != nil {
		return -1, errors.Annotatef(err, "cannot execute command template %q", t.Name())
	}
	command := buf.String()
	logger.Debugf("running command %q", command)
	result, err := exec.RunCommands(exec.RunParams{Commands: command})
	if err != nil {
		return -1, errors.Annotatef(err, "cannot run command %q", command)
	}
	exitCode = result.Code
	stdout := string(result.Stdout)
	stderr := string(result.Stderr)
	logger.Debugf(
		"command %q returned code=%d, stdout=%q, stderr=%q",
		command, exitCode, stdout, stderr,
	)
	if exitCode != 0 {
		if exitNonZeroOK {
			return exitCode, nil
		}
		return exitCode, errors.Errorf(
			"command %q failed with exit code %d",
			command, exitCode,
		)
	}
	return 0, nil
}

// setupRoutesAndIPTables sets up on the host machine the needed
// iptables rules and static routes for an addressable container.
var setupRoutesAndIPTables = func(
	primaryNIC string,
	primaryAddr network.Address,
	bridgeName string,
	ifaceInfo []network.InterfaceInfo,
	enableNAT bool,
) error {

	if primaryNIC == "" || primaryAddr.Value == "" || bridgeName == "" || len(ifaceInfo) == 0 {
		return errors.Errorf("primaryNIC, primaryAddr, bridgeName, and ifaceInfo must be all set")
	}

	for _, iface := range ifaceInfo {
		containerIP := iface.Address.Value
		if containerIP == "" {
			return errors.Errorf("container IP %q must be set", containerIP)
		}
		data := struct {
			HostIF        string
			HostIP        string
			HostBridge    string
			ContainerIP   string
			ContainerCIDR string
			SubnetCIDR    string
		}{primaryNIC, primaryAddr.Value, bridgeName, containerIP, iface.CIDR, iface.CIDR}

		var addRuleIfDoesNotExist = func(name string, rule IptablesRule) error {
			check := mustExecTemplate("rule", "iptables -t {{.Table}} -C {{.Chain}} {{.Rule}}", rule)
			t := mustParseTemplate(name+"Check", check)

			code, err := runTemplateCommand(t, true, data)
			if err != nil {
				return errors.Trace(err)
			}
			switch code {
			case 0:
			// Rule does exist. Do nothing
			case 1:
				// Rule does not exist, add it. We insert the rule at the top of the list so it precedes any
				// REJECT rules.
				action := mustExecTemplate("action", "iptables -t {{.Table}} -I {{.Chain}} 1 {{.Rule}}", rule)
				t = mustParseTemplate(name+"Add", action)
				_, err = runTemplateCommand(t, false, data)
				if err != nil {
					return errors.Trace(err)
				}
			default:
				// Unexpected code - better report it.
				return errors.Errorf("iptables failed with unexpected exit code %d", code)
			}
			return nil
		}

		for name, rule := range iptablesRules {
			if !enableNAT && name == "iptablesSNAT" {
				// Do not add the SNAT rule if we shouldn't enable
				// NAT.
				continue
			}
			if err := addRuleIfDoesNotExist(name, rule); err != nil {
				return err
			}
		}

		// TODO(dooferlad): subnets should be a list of subnets in the EC2 VPC and
		// should be empty for MAAS. See bug http://pad.lv/1443942
		if enableNAT {
			// Only add the following hack to allow AWS egress traffic
			// for hosted containers to work.
			subnets := []string{data.HostIP + "/16"}
			for _, subnet := range subnets {
				data.SubnetCIDR = subnet
				if err := addRuleIfDoesNotExist("skipSNAT", skipSNATRule); err != nil {
					return err
				}
			}
		}

		code, err := runTemplateCommand(ipRouteAdd, false, data)
		// Ignore errors if the exit code was 2, which signals that the route was not added
		// because it already exists.
		if code != 2 && err != nil {
			return errors.Trace(err)
		}
		if code == 2 {
			logger.Tracef("route already exists - not added")
		} else {
			logger.Tracef("route added: container uses host network interface")
		}
	}
	logger.Infof("successfully configured iptables and routes for container interfaces")

	return nil
}

var (
	netInterfaceByName = net.InterfaceByName
	netInterfaces      = net.Interfaces
	interfaceAddrs     = (*net.Interface).Addrs
)

func discoverPrimaryNIC() (string, network.Address, error) {
	interfaces, err := netInterfaces()
	if err != nil {
		return "", network.Address{}, errors.Annotatef(err, "cannot get network interfaces")
	}
	logger.Tracef("trying to discover primary network interface")
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 {
			// Skip the loopback.
			logger.Tracef("not using loopback interface %q", iface.Name)
			continue
		}
		if iface.Flags&net.FlagUp != 0 {
			// Possibly the primary, but ensure it has an address as
			// well.
			logger.Tracef("verifying interface %q has addresses", iface.Name)
			addrs, err := interfaceAddrs(&iface)
			if err != nil {
				return "", network.Address{}, errors.Annotatef(err, "cannot get %q addresses", iface.Name)
			}
			if len(addrs) > 0 {
				// We found it.
				// Check if it's an IP or a CIDR.
				addr := addrs[0].String()
				ip := net.ParseIP(addr)
				if ip == nil {
					// Try a CIDR.
					ip, _, err = net.ParseCIDR(addr)
					if err != nil {
						return "", network.Address{}, errors.Annotatef(err, "cannot parse address %q", addr)
					}
				}
				addr = ip.String()

				logger.Tracef("primary network interface is %q, address %q", iface.Name, addr)
				return iface.Name, network.NewAddress(addr), nil
			}
		}
	}
	return "", network.Address{}, errors.Errorf("cannot detect the primary network interface")
}

// configureContainerNetworking tries to allocate a static IP address
// for the given containerId using the provisioner API, when
// allocateAddress is true. Otherwise it configures the container with
// an already allocated address, when allocateAddress is false (e.g.
// after a host reboot). If the API call fails, it's not critical -
// just a warning, and it won't cause StartInstance to fail.
func configureContainerNetwork(
	containerId, bridgeDevice string,
	apiFacade APICalls,
	ifaceInfo []network.InterfaceInfo,
	allocateAddress bool,
	enableNAT bool,
) (finalIfaceInfo []network.InterfaceInfo, err error) {
	defer func() {
		if err != nil {
			logger.Warningf(
				"failed configuring a static IP for container %q: %v",
				containerId, err,
			)
		}
	}()

	if len(ifaceInfo) != 0 {
		// When we already have interface info, don't overwrite it.
		return nil, nil
	}

	var primaryNIC string
	var primaryAddr network.Address
	primaryNIC, primaryAddr, err = discoverPrimaryNIC()
	if err != nil {
		return nil, errors.Trace(err)
	}

	if allocateAddress {
		logger.Debugf("trying to allocate a static IP for container %q", containerId)
		finalIfaceInfo, err = apiFacade.PrepareContainerInterfaceInfo(names.NewMachineTag(containerId))
	} else {
		logger.Debugf("getting allocated static IP for container %q", containerId)
		finalIfaceInfo, err = apiFacade.GetContainerInterfaceInfo(names.NewMachineTag(containerId))
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	logger.Debugf("container interface info result %#v", finalIfaceInfo)

	// Populate ConfigType and DNSServers as needed.
	var dnsServers []network.Address
	var searchDomain string
	dnsServers, searchDomain, err = localDNSServers()
	if err != nil {
		return nil, errors.Trace(err)
	}
	// Generate the final configuration for each container interface.
	for i, _ := range finalIfaceInfo {
		// Always start at the first device index and generate the
		// interface name based on that. We need to do this otherwise
		// the container will inherit the host's device index and
		// interface name.
		finalIfaceInfo[i].DeviceIndex = i
		finalIfaceInfo[i].InterfaceName = fmt.Sprintf("eth%d", i)
		finalIfaceInfo[i].ConfigType = network.ConfigStatic
		finalIfaceInfo[i].DNSServers = dnsServers
		finalIfaceInfo[i].DNSSearchDomains = []string{searchDomain}
		finalIfaceInfo[i].GatewayAddress = primaryAddr
	}
	err = setupRoutesAndIPTables(
		primaryNIC,
		primaryAddr,
		bridgeDevice,
		finalIfaceInfo,
		enableNAT,
	)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return finalIfaceInfo, nil
}

func prepareOrGetContainerInterfaceInfo(
	api APICalls,
	machineID string,
	bridgeDevice string,
	allocateOrMaintain bool,
	enableNAT bool,
	startingNetworkInfo []network.InterfaceInfo,
	log loggo.Logger,
	providerType string,
) ([]network.InterfaceInfo, error) {
	maintain := !allocateOrMaintain

	if environs.AddressAllocationEnabled(providerType) {
		if maintain {
			log.Debugf("running maintenance for container %q", machineID)
		} else {
			log.Debugf("trying to allocate static IP for container %q", machineID)
		}

		allocatedInfo, err := configureContainerNetwork(
			machineID,
			bridgeDevice,
			api,
			startingNetworkInfo,
			allocateOrMaintain,
			enableNAT,
		)
		if err != nil && !maintain {
			log.Infof("not allocating static IP for container %q: %v", machineID, err)
		}
		return allocatedInfo, err
	}

	if maintain {
		log.Debugf("address allocation disabled: Not running maintenance for machine %q", machineID)
		return nil, nil
	}

	log.Debugf("address allocation feature flag not enabled; using multi-bridge networking for container %q", machineID)

	containerTag := names.NewMachineTag(machineID)
	preparedInfo, err := api.PrepareContainerInterfaceInfo(containerTag)
	if err != nil && errors.IsNotSupported(err) {
		log.Warningf("%v (using fallback config)", err)
	} else if err != nil {
		return nil, errors.Trace(err)
	}
	log.Tracef("PrepareContainerInterfaceInfo returned %+v", preparedInfo)

	// Use the fallback network config as a last resort.
	if len(preparedInfo) == 0 {
		log.Infof("using fallback network config for container %q", machineID)
		preparedInfo = container.FallbackInterfaceInfo()
	}

	dnsServersFound := false
	for _, info := range preparedInfo {
		if len(info.DNSServers) > 0 {
			dnsServersFound = true
			break
		}
	}
	if !dnsServersFound {
		logger.Warningf("no DNS settings found, discovering the host settings")
		dnsServers, searchDomain, err := localDNSServers()
		if err != nil {
			return nil, errors.Trace(err)
		}

		// Since the result is sorted, the first entry is the primary NIC.
		preparedInfo[0].DNSServers = dnsServers
		preparedInfo[0].DNSSearchDomains = []string{searchDomain}
		logger.Debugf(
			"setting DNS servers %+v and domains %+v on container interface %q",
			preparedInfo[0].DNSServers, preparedInfo[0].DNSSearchDomains, preparedInfo[0].InterfaceName,
		)
	}

	return preparedInfo, nil
}

func maybeReleaseContainerAddresses(
	api APICalls,
	instanceID instance.Id,
	namespace instance.Namespace,
	log loggo.Logger,
	providerType string,
) {
	if environs.AddressAllocationEnabled(providerType) {
		// The addresser worker will take care of the addresses.
		return
	}
	// If we're not using addressable containers, we might still have used MAAS
	// 1.8+ device to register the container when provisioning. In that case we
	// need to attempt releasing the device, but ignore a NotSupported error
	// (when we're not using MAAS 1.8+).
	containerTag, err := namespace.MachineTag(string(instanceID))
	if err != nil {
		// Not a reason to cause StopInstances to fail though..
		log.Warningf("unexpected container tag %q: %v", instanceID, err)
		return
	}
	err = api.ReleaseContainerAddresses(containerTag)
	switch {
	case err == nil:
		log.Infof("released all addresses for container %q", containerTag.Id())
	case errors.IsNotSupported(err):
		log.Warningf("not releasing all addresses for container %q: %v", containerTag.Id(), err)
	default:
		log.Warningf(
			"unexpected error trying to release container %q addreses: %v",
			containerTag.Id(), err,
		)
	}
}

// matchHostArchTools filters the given list of tools to the host architecture.
func matchHostArchTools(allTools tools.List) (tools.List, error) {
	arch := arch.HostArch()
	archTools, err := allTools.Match(tools.Filter{Arch: arch})
	if err == tools.ErrNoMatches {
		return nil, errors.Errorf(
			"need tools for arch %s, only found %s",
			arch, allTools.Arches(),
		)
	} else if err != nil {
		return nil, errors.Trace(err)
	}
	return archTools, nil
}