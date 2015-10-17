// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package state_test

import (
	"reflect"

	"github.com/juju/errors"
	"github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/charm.v5"

	"github.com/juju/juju/workload"
	"github.com/juju/juju/workload/state"
)

var _ = gc.Suite(&envPayloadsSuite{})

type envPayloadsSuite struct {
	baseWorkloadsSuite

	persists *stubPayloadsPersistence
}

func (s *envPayloadsSuite) SetUpTest(c *gc.C) {
	s.baseWorkloadsSuite.SetUpTest(c)

	s.persists = &stubPayloadsPersistence{stub: s.stub}
}

func (s *envPayloadsSuite) newPayload(name string) workload.Payload {
	return workload.Payload{
		PayloadClass: charm.PayloadClass{
			Name: name,
			Type: "docker",
		},
		ID:      "id" + name,
		Status:  workload.StateRunning,
		Tags:    []string{"a-tag"},
		Unit:    "unit-a-service-0",
		Machine: "1",
	}
}

func (s *envPayloadsSuite) TestListAllOkay(c *gc.C) {
	p1 := s.newPayload("spam")
	p2 := s.newPayload("eggs")
	s.persists.setPayloads(p1, p2)

	ps := state.EnvPayloads{
		UnitListFuncs: s.persists.listFuncs,
	}
	payloads, err := ps.ListAll()
	c.Assert(err, jc.ErrorIsNil)

	s.stub.CheckCallNames(c, "ListAll")
	checkPayloads(c, payloads, []workload.Payload{
		p1,
		p2,
	})
}

func (s *envPayloadsSuite) TestListAllMulti(c *gc.C) {
	p1 := s.newPayload("spam")
	p2 := s.newPayload("eggs")
	p2.Unit = "unit-a-service-1"
	p3 := s.newPayload("ham")
	p3.Unit = "unit-a-service-2"
	p3.Machine = "2"
	p4 := s.newPayload("spamspamspam")
	p4.Unit = "unit-a-service-1"
	s.persists.setPayloads(p1, p2, p3, p4)

	ps := state.EnvPayloads{
		UnitListFuncs: s.persists.listFuncs,
	}
	payloads, err := ps.ListAll()
	c.Assert(err, jc.ErrorIsNil)

	s.stub.CheckCallNames(c, "ListAll", "ListAll", "ListAll")
	checkPayloads(c, payloads, []workload.Payload{
		p1,
		p2,
		p3,
		p4,
	})
}

func (s *envPayloadsSuite) TestListAllFailed(c *gc.C) {
	failure := errors.Errorf("<failed!>")
	s.stub.SetErrors(failure)
	p1 := s.newPayload("spam")
	p2 := s.newPayload("eggs")
	s.persists.setPayloads(p1, p2)

	ps := state.EnvPayloads{
		UnitListFuncs: s.persists.listFuncs,
	}
	_, err := ps.ListAll()

	s.stub.CheckCallNames(c, "ListAll")
	c.Check(errors.Cause(err), gc.Equals, failure)
}

func checkPayloads(c *gc.C, payloads, expectedList []workload.Payload) {
	remainder := make([]workload.Payload, len(payloads))
	copy(remainder, payloads)
	var noMatch []workload.Payload
	for _, expected := range expectedList {
		found := false
		for i, payload := range remainder {
			if reflect.DeepEqual(payload, expected) {
				remainder = append(remainder[:i], remainder[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			noMatch = append(noMatch, expected)
		}
	}

	ok1 := c.Check(noMatch, gc.HasLen, 0)
	ok2 := c.Check(remainder, gc.HasLen, 0)
	if !ok1 || !ok2 {
		c.Logf("<<<<<<<<\nexpected:")
		for _, payload := range expectedList {
			c.Logf("%#v", payload)
		}
		c.Logf("--------\ngot:")
		for _, payload := range payloads {
			c.Logf("%#v", payload)
		}
		c.Logf(">>>>>>>>")
	}
}

type stubPayloadsPersistence struct {
	stub *testing.Stub

	persists map[string]map[string]*fakeWorkloadsPersistence
}

func (s *stubPayloadsPersistence) listFuncs() ([]func() ([]workload.Info, string, string, error), error) {
	var funcs []func() ([]workload.Info, string, string, error)
	for machine, units := range s.persists {
		for unit, unitWorkloads := range units {
			pmachine := machine
			punit := unit
			persist := unitWorkloads
			funcs = append(funcs, func() ([]workload.Info, string, string, error) {
				workloads, err := persist.ListAll()
				if err != nil {
					return nil, "", "", errors.Trace(err)
				}
				return workloads, pmachine, punit, nil
			})
		}
	}
	return funcs, nil
}

func (s *stubPayloadsPersistence) checkPayloads(c *gc.C, expectedList []workload.Payload) {
	collated := make(map[*fakeWorkloadsPersistence][]workload.Info)
	for _, payload := range expectedList {
		unitWorkloads := s.persists[payload.Machine][payload.Unit]
		workload := payload.AsWorkload()
		collated[unitWorkloads] = append(collated[unitWorkloads], workload)
	}

	for unitWorkloads, workloads := range collated {
		unitWorkloads.checkWorkloads(c, workloads)
	}
}

func (s *stubPayloadsPersistence) setPayloads(payloads ...workload.Payload) {
	if len(payloads) > 0 && s.persists == nil {
		s.persists = make(map[string]map[string]*fakeWorkloadsPersistence)
	}

	for _, payload := range payloads {
		workload := payload.AsWorkload()

		units := s.persists[payload.Machine]
		if units == nil {
			units = make(map[string]*fakeWorkloadsPersistence)
			s.persists[payload.Machine] = units
		}
		unitWorkloads := units[payload.Unit]
		if unitWorkloads == nil {
			unitWorkloads = &fakeWorkloadsPersistence{Stub: s.stub}
			units[payload.Unit] = unitWorkloads
		}

		unitWorkloads.setWorkloads(&workload)
	}
}