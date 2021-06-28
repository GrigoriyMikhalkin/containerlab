// Copyright 2020 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package ceos

import (
	"context"
	_ "embed"
	"fmt"
	"net"
	"path"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/srl-labs/containerlab/nodes"
	"github.com/srl-labs/containerlab/runtime"
	"github.com/srl-labs/containerlab/types"
	"github.com/srl-labs/containerlab/utils"
)

// defined env vars for the ceos
var ceosEnv = map[string]string{
	"CEOS":                                "1",
	"EOS_PLATFORM":                        "ceoslab",
	"container":                           "docker",
	"ETBA":                                "4",
	"SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT": "1",
	"INTFTYPE":                            "eth",
	"MAPETH0":                             "1",
	"MGMT_INTF":                           "eth0",
}

//go:embed ceos.cfg
var cfgTemplate string

func init() {
	nodes.Register(nodes.NodeKindCEOS, func() nodes.Node {
		return new(ceos)
	})
}

type ceos struct {
	cfg *types.NodeConfig
}

func (s *ceos) Init(cfg *types.NodeConfig, opts ...nodes.NodeOption) error {
	s.cfg = cfg
	for _, o := range opts {
		o(s)
	}

	s.cfg.Env = utils.MergeStringMaps(ceosEnv, s.cfg.Env)

	// the node.Cmd should be aligned with the environment.
	var envSb strings.Builder
	envSb.WriteString("/sbin/init ")
	for k, v := range s.cfg.Env {
		envSb.WriteString("systemd.setenv=" + k + "=" + v + " ")
	}
	s.cfg.Cmd = envSb.String()
	s.cfg.MacAddress = utils.GenMac("00:1c:73")

	// mount config dir
	cfgPath := filepath.Join(s.cfg.LabDir, "flash")
	s.cfg.Binds = append(s.cfg.Binds, fmt.Sprintf("%s:/mnt/flash/", cfgPath))
	return nil
}

func (s *ceos) Config() *types.NodeConfig { return s.cfg }

func (s *ceos) PreDeploy(configName, labCADir, labCARoot string) error {
	utils.CreateDirectory(s.cfg.LabDir, 0777)
	return createCEOSFiles(s.cfg)
}

func (s *ceos) Deploy(ctx context.Context, r runtime.ContainerRuntime) error {
	return r.CreateContainer(ctx, s.cfg)
}

func (s *ceos) PostDeploy(ctx context.Context, r runtime.ContainerRuntime, ns map[string]nodes.Node) error {
	log.Debugf("Running postdeploy actions for Arista cEOS '%s' node", s.cfg.ShortName)
	return ceosPostDeploy(ctx, r, s.cfg)
}

func (s *ceos) WithMgmtNet(*types.MgmtNet) {}

//

func createCEOSFiles(node *types.NodeConfig) error {
	// generate config directory
	utils.CreateDirectory(path.Join(node.LabDir, "flash"), 0777)
	cfg := path.Join(node.LabDir, "flash", "startup-config")
	node.ResConfig = cfg

	// sysmac is a system mac that is +1 to Ma0 mac
	m, err := net.ParseMAC(node.MacAddress)
	if err != nil {
		return err
	}
	m[5] = m[5] + 1
	utils.CreateFile(path.Join(node.LabDir, "flash", "system_mac_address"), m.String())
	return nil
}

func ceosPostDeploy(ctx context.Context, r runtime.ContainerRuntime, nodeCfg *types.NodeConfig) error {
	// regenerate ceos config since it is now known which IP address docker assigned to this container
	err := nodeCfg.GenerateConfig(nodeCfg.ResConfig, cfgTemplate)
	if err != nil {
		return err
	}
	log.Infof("Restarting '%s' node", nodeCfg.ShortName)
	// force stopping and start is faster than ContainerRestart
	var timeout time.Duration = 1
	err = r.StopContainer(ctx, nodeCfg.ContainerID, &timeout)
	if err != nil {
		return err
	}
	// remove the netns symlink created during original start
	// we will re-symlink it later
	if err := utils.DeleteNetnsSymlink(nodeCfg.LongName); err != nil {
		return err
	}
	err = r.StartContainer(ctx, nodeCfg.ContainerID)
	if err != nil {
		return err
	}
	nodeCfg.NSPath, err = r.GetNSPath(ctx, nodeCfg.ContainerID)
	if err != nil {
		return err
	}
	return utils.LinkContainerNS(nodeCfg.NSPath, nodeCfg.LongName)
}
