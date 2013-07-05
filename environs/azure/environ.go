// Copyright 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package azure

import (
	"fmt"
	"net/http"
	"sync"

	"launchpad.net/gwacl"
	"launchpad.net/juju-core/constraints"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/cloudinit"
	"launchpad.net/juju-core/environs/config"
	"launchpad.net/juju-core/environs/tools"
	"launchpad.net/juju-core/instance"
	"launchpad.net/juju-core/log"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api"
)

// In our initial implementation, each instance gets its own Azure
// hosted service.  Once we have a DNS name, we write it into the
// Label field on the hosted service as a shortcut.  This will have
// to change once we suppport multiple instances per hosted service.
// (instance==service).
// This label is a placeholder to say "still waiting for DNS."
const noDNSLabel = "(Waiting for DNS name)"

type azureEnviron struct {
	// Except where indicated otherwise, all fields in this object should
	// only be accessed using a lock or a snapshot.
	sync.Mutex

	// name is immutable; it does not need locking.
	name string

	// ecfg is the environment's Azure-specific configuration.
	ecfg *azureEnvironConfig

	// storage is this environ's own private storage.
	storage environs.Storage

	// publicStorage is the public storage that this environ uses.
	publicStorage environs.StorageReader
}

// azureEnviron implements Environ.
var _ environs.Environ = (*azureEnviron)(nil)

// NewEnviron creates a new azureEnviron.
func NewEnviron(cfg *config.Config) (*azureEnviron, error) {
	env := azureEnviron{name: cfg.Name()}
	err := env.SetConfig(cfg)
	if err != nil {
		return nil, err
	}

	// Set up storage.
	env.storage = &azureStorage{
		storageContext: &environStorageContext{environ: &env},
	}

	// Set up public storage.
	publicContext := publicEnvironStorageContext{environ: &env}
	if publicContext.getContainer() == "" {
		// No public storage configured.  Use EmptyStorage.
		env.publicStorage = environs.EmptyStorage
	} else {
		// Set up real public storage.
		env.publicStorage = &azureStorage{storageContext: &publicContext}
	}

	return &env, nil
}

// Name is specified in the Environ interface.
func (env *azureEnviron) Name() string {
	return env.name
}

// getSnapshot produces an atomic shallow copy of the environment object.
// Whenever you need to access the environment object's fields without
// modifying them, get a snapshot and read its fields instead.  You will
// get a consistent view of the fields without any further locking.
// If you do need to modify the environment's fields, do not get a snapshot
// but lock the object throughout the critical section.
func (env *azureEnviron) getSnapshot() *azureEnviron {
	env.Lock()
	defer env.Unlock()

	// Copy the environment.  (Not the pointer, the environment itself.)
	// This is a shallow copy.
	snap := *env
	// Reset the snapshot's mutex, because we just copied it while we
	// were holding it.  The snapshot will have a "clean," unlocked mutex.
	snap.Mutex = sync.Mutex{}
	return &snap
}

// Bootstrap is specified in the Environ interface.
func (env *azureEnviron) Bootstrap(cons constraints.Value) error {
	panic("unimplemented")
}

// StateInfo is specified in the Environ interface.
func (env *azureEnviron) StateInfo() (*state.Info, *api.Info, error) {
	return environs.StateInfo(env)
}

// Config is specified in the Environ interface.
func (env *azureEnviron) Config() *config.Config {
	snap := env.getSnapshot()
	return snap.ecfg.Config
}

// SetConfig is specified in the Environ interface.
func (env *azureEnviron) SetConfig(cfg *config.Config) error {
	ecfg, err := azureEnvironProvider{}.newConfig(cfg)
	if err != nil {
		return err
	}

	env.Lock()
	defer env.Unlock()

	if env.ecfg != nil {
		_, err = azureEnvironProvider{}.Validate(cfg, env.ecfg.Config)
		if err != nil {
			return err
		}
	}

	env.ecfg = ecfg
	return nil
}

// attemptCreateService tries to create a new hosted service on Azure, with a
// name it chooses, but recognizes that the name may not be available.  If
// the name is not available, it does not treat that as an error but just
// returns nil.
func attemptCreateService(azure *gwacl.ManagementAPI) (*gwacl.CreateHostedService, error) {
	// Initially, this is the only location where Azure supports Linux.
	const location = "East US"

	name := gwacl.MakeRandomHostedServiceName("juju")
	req := gwacl.NewCreateHostedServiceWithLocation(name, noDNSLabel, location)
	err := azure.AddHostedService(req)
	azErr, isAzureError := err.(*gwacl.AzureError)
	if isAzureError && azErr.HTTPStatus == http.StatusConflict {
		// Conflict.  As far as we can see, this only happens if the
		// name was already in use.  It's still dangerous to assume
		// that we know it can't be anything else, but there's nothing
		// else in the error that we can use for closer identifcation.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return req, nil
}

// newHostedService creates a hosted service.  It will make up a unique name.
func newHostedService(azure *gwacl.ManagementAPI) (*gwacl.CreateHostedService, error) {
	var err error
	var svc *gwacl.CreateHostedService
	for tries := 10; tries > 0 && err == nil && svc == nil; tries-- {
		svc, err = attemptCreateService(azure)
	}
	if err != nil {
		return nil, fmt.Errorf("could not create hosted service: %v", err)
	}
	if svc == nil {
		return nil, fmt.Errorf("could not come up with a unique hosted service name - is your randomizer initialized?")
	}
	return svc, nil
}

func extractDeploymentHostname(url string) (string, error) {
	// TODO: Implement!
	return "", nil
}

func setServiceDNSName(azure *gwacl.ManagementAPI, serviceName, deploymentName string) error {
	deployment, err := azure.GetDeployment(&gwacl.GetDeploymentRequest{
		ServiceName:    serviceName,
		DeploymentName: deploymentName,
	})
	if err != nil {
		return fmt.Errorf("could not read newly created deployment: %v", err)
	}
	hostname, err := extractDeploymentHostname(deployment.URL)
	if err != nil {
		return err
	}

	update := gwacl.NewUpdateHostedService(hostname, "Juju instance", nil)
	return azure.UpdateHostedService(serviceName, update)
}

// internalStartInstance does the provider-specific work of starting an
// instance.  The code in StartInstance is actually largely agnostic across
// the EC2/OpenStack/MAAS/Azure providers.
func (env *azureEnviron) internalStartInstance(machineID string, cons constraints.Value, possibleTools tools.List, mcfg *cloudinit.MachineConfig) (_ instance.Instance, err error) {
	// Declaring "err" in the function signature so that we can "defer"
	// any cleanup that needs to run during error returns.

	series := possibleTools.Series()
	if len(series) != 1 {
		return nil, fmt.Errorf("expected single series, got %v", series)
	}

	err = environs.FinishMachineConfig(mcfg, env.Config(), cons)
	if err != nil {
		return nil, err
	}

	// TODO: Compose userdata.

	azure, err := env.getManagementAPI()
	if err != nil {
		return nil, err
	}
	defer env.releaseManagementAPI(azure)

	createdService, err := newHostedService(azure.ManagementAPI)
	if err != nil {
		return nil, err
	}

	// If we fail after this point, clean up the hosted service.
	defer func() {
		if err != nil {
			azure.DeleteHostedService(createdService.ServiceName)
		}
	}()

	// TODO: Create VM Deployment.
	var deployment *gwacl.Deployment

	var inst instance.Instance
	// TODO: Make sure ssh port is open.

	// From here on, remember to shut down the instance before returning
	// any error.
	defer func() {
		if err != nil && inst != nil {
			err2 := env.StopInstances([]instance.Instance{inst})
			if err2 != nil {
				// Failure upon failure.  Log it, but return
				// the original error.
				log.Errorf("error releasing failed instance: %v", err)
			}
		}
	}()

	err = setServiceDNSName(azure.ManagementAPI, createdService.ServiceName, deployment.Name)
	if err != nil {
		return nil, fmt.Errorf("could not set instance DNS name as service label: %v", err)
	}

	return inst, nil
}

// StartInstance is specified in the Environ interface.
func (env *azureEnviron) StartInstance(machineId, machineNonce string, series string, cons constraints.Value,
	info *state.Info, apiInfo *api.Info) (instance.Instance, *instance.HardwareCharacteristics, error) {
	panic("unimplemented")
}

// StopInstances is specified in the Environ interface.
func (env *azureEnviron) StopInstances([]instance.Instance) error {
	panic("unimplemented")
}

// Instances is specified in the Environ interface.
func (env *azureEnviron) Instances(ids []instance.Id) ([]instance.Instance, error) {
	// If the list of ids is empty, return nil as specified by the
	// interface
	if len(ids) == 0 {
		return nil, environs.ErrNoInstances
	}
	// Acquire management API object.
	context, err := env.getManagementAPI()
	if err != nil {
		return nil, err
	}
	defer env.releaseManagementAPI(context)

	// Prepare gwacl request object.
	container := env.getSnapshot().ecfg.StorageContainerName()
	deploymentNames := make([]string, len(ids))
	for i, id := range ids {
		deploymentNames[i] = string(id)
	}
	request := &gwacl.ListDeploymentsRequest{ServiceName: container, DeploymentNames: deploymentNames}

	// Issue 'ListDeployments' request with gwacl.
	deployments, err := context.ListDeployments(request)
	if err != nil {
		return nil, err
	}

	// If no instances were found, return ErrNoInstances.
	if len(deployments) == 0 {
		return nil, environs.ErrNoInstances
	}

	instances := convertToInstances(deployments)

	// Check if we got a partial result.
	if len(ids) != len(instances) {
		return instances, environs.ErrPartialInstances
	}
	return instances, nil
}

// AllInstances is specified in the Environ interface.
func (env *azureEnviron) AllInstances() ([]instance.Instance, error) {
	// Acquire management API object.
	context, err := env.getManagementAPI()
	if err != nil {
		return nil, err
	}
	defer env.releaseManagementAPI(context)

	container := env.getSnapshot().ecfg.StorageContainerName()
	request := &gwacl.ListAllDeploymentsRequest{ServiceName: container}
	deployments, err := context.ListAllDeployments(request)
	if err != nil {
		return nil, err
	}
	return convertToInstances(deployments), nil
}

// convertToInstances converts a slice of gwacl.Deployment objects into
// a slice of instance.Instance objects.
func convertToInstances(deployments []gwacl.Deployment) []instance.Instance {
	instances := make([]instance.Instance, len(deployments))
	for i, deployment := range deployments {
		instances[i] = &azureInstance{deployment}
	}
	return instances
}

// Storage is specified in the Environ interface.
func (env *azureEnviron) Storage() environs.Storage {
	return env.getSnapshot().storage
}

// PublicStorage is specified in the Environ interface.
func (env *azureEnviron) PublicStorage() environs.StorageReader {
	return env.getSnapshot().publicStorage
}

// Destroy is specified in the Environ interface.
func (env *azureEnviron) Destroy(insts []instance.Instance) error {
	panic("unimplemented")
}

// OpenPorts is specified in the Environ interface.
func (env *azureEnviron) OpenPorts(ports []instance.Port) error {
	panic("unimplemented")
}

// ClosePorts is specified in the Environ interface.
func (env *azureEnviron) ClosePorts(ports []instance.Port) error {
	panic("unimplemented")
}

// Ports is specified in the Environ interface.
func (env *azureEnviron) Ports() ([]instance.Port, error) {
	panic("unimplemented")
}

// Provider is specified in the Environ interface.
func (env *azureEnviron) Provider() environs.EnvironProvider {
	panic("unimplemented")
}

// azureManagementContext wraps two things: a gwacl.ManagementAPI (effectively
// a session on the Azure management API) and a tempCertFile, which keeps track
// of the temporary certificate file that needs to be deleted once we're done
// with this particular session.
// Since it embeds *gwacl.ManagementAPI, you can use it much as if it were a
// pointer to a ManagementAPI object.  Just don't forget to release it after
// use.
type azureManagementContext struct {
	*gwacl.ManagementAPI
	certFile *tempCertFile
}

// getManagementAPI obtains a context object for interfacing with Azure's
// management API.
// For now, each invocation just returns a separate object.  This is probably
// wasteful (each context gets its own SSL connection) and may need optimizing
// later.
func (env *azureEnviron) getManagementAPI() (*azureManagementContext, error) {
	snap := env.getSnapshot()
	subscription := snap.ecfg.ManagementSubscriptionId()
	certData := snap.ecfg.ManagementCertificate()
	certFile, err := newTempCertFile([]byte(certData))
	if err != nil {
		return nil, err
	}
	// After this point, if we need to leave prematurely, we should clean
	// up that certificate file.
	mgtAPI, err := gwacl.NewManagementAPI(subscription, certFile.Path())
	if err != nil {
		certFile.Delete()
		return nil, err
	}
	context := azureManagementContext{
		ManagementAPI: mgtAPI,
		certFile:      certFile,
	}
	return &context, nil
}

// releaseManagementAPI frees up a context object obtained through
// getManagementAPI.
func (env *azureEnviron) releaseManagementAPI(context *azureManagementContext) {
	// Be tolerant to incomplete context objects, in case we ever get
	// called during cleanup of a failed attempt to create one.
	if context == nil || context.certFile == nil {
		return
	}
	// For now, all that needs doing is to delete the temporary certificate
	// file.  We may do cleverer things later, such as connection pooling
	// where this method returns a context to the pool.
	context.certFile.Delete()
}

// getStorageContext obtains a context object for interfacing with Azure's
// storage API.
// For now, each invocation just returns a separate object.  This is probably
// wasteful (each context gets its own SSL connection) and may need optimizing
// later.
func (env *azureEnviron) getStorageContext() (*gwacl.StorageContext, error) {
	ecfg := env.getSnapshot().ecfg
	context := gwacl.StorageContext{
		Account: ecfg.StorageAccountName(),
		Key:     ecfg.StorageAccountKey(),
	}
	// There is currently no way for this to fail.
	return &context, nil
}

// getPublicStorageContext obtains a context object for interfacing with
// Azure's storage API (public storage).
func (env *azureEnviron) getPublicStorageContext() (*gwacl.StorageContext, error) {
	ecfg := env.getSnapshot().ecfg
	context := gwacl.StorageContext{
		Account: ecfg.PublicStorageAccountName(),
		Key:     "", // Empty string means anonymous access.
	}
	// There is currently no way for this to fail.
	return &context, nil
}
