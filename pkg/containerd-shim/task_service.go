//go:build linux
// +build linux

// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package containerdshim

import (
	"context"
	"sync"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	containerdShim "github.com/urunc-dev/urunc/pkg/containerd-shim/containerd"
	"github.com/urunc-dev/urunc/pkg/unikontainers"
)

// taskService is urunc's shim-side wrapper around containerd's runc task
// service. It wires urunc task setup before forwarding calls to the wrapped
// service.
//
// It also supervises the optional per-container vmi introspection sidecar: the
// config is parsed at Create (bundle available), the sidecar is spawned at Start
// (VMM pid available) and reaped at Delete. The shim is the natural supervisor
// because it is the only runtime process resident for the container's lifetime
// (urunc itself execs into the VMM and is gone).
type taskService struct {
	taskAPI.TaskService

	containerdAddress string

	// vmiMu guards the two maps below, keyed by container ID.
	vmiMu       sync.Mutex
	vmiCfg      map[string]unikontainers.VMIConfig
	vmiSidecars map[string]*unikontainers.VMISidecar
}

func (s *taskService) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	session, err := containerdShim.OpenSession(ctx, s.containerdAddress, r.ID)
	if err != nil {
		log.G(ctx).WithError(err).Warn("urunc(shim): failed to open containerd session")
	} else {
		defer func() {
			if err := session.Close(); err != nil {
				log.G(ctx).WithError(err).Warn("urunc(shim): failed to close containerd session")
			}
		}()
		if err := containerdShim.InjectUruncAnnotations(ctx, session, r.Bundle); err != nil {
			log.G(ctx).WithError(err).Warn("urunc(shim): failed to inject annotations to spec")
		}
	}

	resp, err := s.TaskService.Create(ctx, r)
	if err != nil {
		return resp, err
	}

	// Parse the optional vmi introspection config from the bundle spec and stash
	// it for Start. Best-effort: a parse failure just disables introspection.
	if cfg, cerr := unikontainers.VMIConfigFromBundle(r.Bundle); cerr != nil {
		log.G(ctx).WithError(cerr).Debug("urunc(shim): no vmi config in bundle")
	} else if cfg.Introspect {
		s.vmiMu.Lock()
		s.vmiCfg[r.ID] = cfg
		s.vmiMu.Unlock()
		log.G(ctx).WithField("container", r.ID).Info("urunc(shim): vmi introspection enabled")
	}

	// TODO: #816 - Restore rootfs choice here once shim integration is complete.
	// For now, rootfs is selected during urunc create (InitialSetup phase).
	// ChooseRootfs after inner task Create so bundle rootfs is mounted;
	// params are persisted in bundle config.json for runtime Exec.
	// if err := chooseGuestRootfs(r); err != nil {
	// 	if errors.Is(err, errGuestRootfsChoiceSkipped) {
	// 		log.G(ctx).WithError(err).Debug("urunc(shim): guest rootfs choice skipped")
	// 		return resp, nil
	// 	}
	// 	log.G(ctx).WithError(err).Warn("urunc(shim): failed to choose guest rootfs")
	// 	return nil, err
	// }

	return resp, nil
}

// Start forwards to the wrapped service, then (for the container init process,
// not an exec) spawns the vmi sidecar against the now-running VMM. Best-effort:
// a sidecar failure is logged and the guest keeps running.
func (s *taskService) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	resp, err := s.TaskService.Start(ctx, r)
	if err != nil {
		return resp, err
	}
	if r.ExecID != "" { // only the init process owns the VMM/introspection
		return resp, nil
	}
	s.vmiMu.Lock()
	cfg, ok := s.vmiCfg[r.ID]
	s.vmiMu.Unlock()
	if !ok || !cfg.Introspect {
		return resp, nil
	}
	sc, serr := unikontainers.SpawnVMISidecar(r.ID, int(resp.Pid), cfg)
	if serr != nil {
		log.G(ctx).WithError(serr).Warn("urunc(shim): vmi sidecar not started (guest unaffected)")
		return resp, nil
	}
	if sc != nil {
		s.vmiMu.Lock()
		s.vmiSidecars[r.ID] = sc
		s.vmiMu.Unlock()
	}
	return resp, nil
}

func (s *taskService) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	if r.ExecID == "" { // tearing down the container init: reap its sidecar
		s.vmiMu.Lock()
		sc := s.vmiSidecars[r.ID]
		delete(s.vmiSidecars, r.ID)
		delete(s.vmiCfg, r.ID)
		s.vmiMu.Unlock()
		sc.Stop() // nil-safe
	}
	return s.TaskService.Delete(ctx, r)
}

func (s *taskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTaskService(server, s)
	return nil
}
