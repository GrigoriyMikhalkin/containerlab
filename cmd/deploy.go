package cmd

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	cfssllog "github.com/cloudflare/cfssl/log"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/srl-labs/containerlab/clab"
	"github.com/srl-labs/containerlab/types"
	"github.com/srl-labs/containerlab/utils"
)

// name of the container management network
var mgmtNetName string

// IPv4/6 address range for container management network
var mgmtIPv4Subnet net.IPNet
var mgmtIPv6Subnet net.IPNet

// reconfigure flag
var reconfigure bool

// max-workers flag
var maxWorkers uint

// deployCmd represents the deploy command
var deployCmd = &cobra.Command{
	Use:          "deploy",
	Short:        "deploy a lab",
	Long:         "deploy a lab based defined by means of the topology definition file\nreference: https://containerlab.srlinux.dev/cmd/deploy/",
	Aliases:      []string{"dep"},
	SilenceUsage: true,
	PreRunE:      sudoCheck,
	RunE: func(cmd *cobra.Command, args []string) error {
		var err error
		if err = topoSet(); err != nil {
			return err
		}
		opts := []clab.ClabOption{
			clab.WithDebug(debug),
			clab.WithTimeout(timeout),
			clab.WithTopoFile(topo),
			clab.WithRuntime(rt, debug, timeout, graceful),
		}
		c := clab.NewContainerLab(opts...)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		setFlags(c.Config)
		log.Debugf("lab Conf: %+v", c.Config)
		// Parse topology information
		if err = c.ParseTopology(); err != nil {
			return err
		}

		// latest version channel
		vCh := make(chan string)
		go getLatestVersion(vCh)

		if reconfigure {
			if err != nil {
				return err
			}
			_ = destroyLab(ctx, c)
			log.Infof("Removing %s directory...", c.Dir.Lab)
			if err := os.RemoveAll(c.Dir.Lab); err != nil {
				return err
			}
		}

		if err = c.CheckTopologyDefinition(ctx); err != nil {
			return err
		}

		if err = c.CheckResources(); err != nil {
			return err
		}

		log.Info("Creating lab directory: ", c.Dir.Lab)
		utils.CreateDirectory(c.Dir.Lab, 0755)

		cfssllog.Level = cfssllog.LevelError
		if debug {
			cfssllog.Level = cfssllog.LevelDebug
		}
		if err := c.CreateRootCA(); err != nil {
			return err
		}

		// create docker network or use existing one
		if err = c.Runtime.CreateNet(ctx); err != nil {
			return err
		}

		numNodes := uint(len(c.Nodes))
		numLinks := uint(len(c.Links))
		nodesMaxWorkers := maxWorkers
		linksMaxWorkers := maxWorkers

		if maxWorkers == 0 {
			nodesMaxWorkers = numNodes
			linksMaxWorkers = numLinks
		}

		if nodesMaxWorkers > numNodes {
			nodesMaxWorkers = numNodes
		}
		if linksMaxWorkers > numLinks {
			linksMaxWorkers = numLinks
		}

		c.CreateNodes(ctx, nodesMaxWorkers)
		c.CreateLinks(ctx, linksMaxWorkers, false)

		// generate graph of the lab topology
		if graph {
			if err = c.GenerateGraph(topo); err != nil {
				log.Error(err)
			}
		}
		log.Debug("containers created, retrieving state and IP addresses...")

		labels = append(labels, "containerlab="+c.Config.Name)
		containers, err := c.Runtime.ListContainers(ctx, labels)
		if err != nil {
			return fmt.Errorf("could not list containers: %v", err)
		}
		if len(containers) == 0 {
			return fmt.Errorf("no containers found")
		}

		log.Debug("enriching nodes with IP information...")
		enrichNodes(containers, c.Nodes, c.Config.Mgmt.Network)

		if err := c.GenerateInventories(); err != nil {
			return err
		}

		var wg sync.WaitGroup
		wg.Add(len(c.Nodes))
		for _, node := range c.Nodes {
			go func(node *types.NodeBase) {
				defer wg.Done()
				err := c.ExecPostDeployTasks(ctx, node, linksMaxWorkers)
				if err != nil {
					log.Errorf("failed to run postdeploy task for node %s: %v", node.ShortName, err)
				}
			}(node)

		}
		wg.Wait()

		// run links postdeploy creation (ceos links creation)
		c.CreateLinks(ctx, linksMaxWorkers, true)

		log.Info("Writing /etc/hosts file")
		err = createHostsFile(containers, c.Config.Mgmt.Network)
		if err != nil {
			log.Errorf("failed to create hosts file: %v", err)
		}

		// log new version availability info if ready
		newVerNotification(vCh)

		// print table summary
		printContainerInspect(c, containers, c.Config.Mgmt.Network, format)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(deployCmd)
	deployCmd.Flags().BoolVarP(&graph, "graph", "g", false, "generate topology graph")
	deployCmd.Flags().StringVarP(&mgmtNetName, "network", "", "", "management network name")
	deployCmd.Flags().IPNetVarP(&mgmtIPv4Subnet, "ipv4-subnet", "4", net.IPNet{}, "management network IPv4 subnet range")
	deployCmd.Flags().IPNetVarP(&mgmtIPv6Subnet, "ipv6-subnet", "6", net.IPNet{}, "management network IPv6 subnet range")
	deployCmd.Flags().BoolVarP(&reconfigure, "reconfigure", "", false, "regenerate configuration artifacts and overwrite the previous ones if any")
	deployCmd.Flags().UintVarP(&maxWorkers, "max-workers", "", 0, "limit the maximum number of workers creating nodes and virtual wires")
}

func setFlags(conf *clab.Config) {
	if name != "" {
		conf.Name = name
	}
	if mgmtNetName != "" {
		conf.Mgmt.Network = mgmtNetName
	}
	if mgmtIPv4Subnet.String() != "<nil>" {
		conf.Mgmt.IPv4Subnet = mgmtIPv4Subnet.String()
	}
	if mgmtIPv6Subnet.String() != "<nil>" {
		conf.Mgmt.IPv6Subnet = mgmtIPv6Subnet.String()
	}
}

func createHostsFile(containers []types.GenericContainer, bridgeName string) error {
	if bridgeName == "" {
		return fmt.Errorf("missing bridge name")
	}
	data := hostsEntries(containers, bridgeName)
	if len(data) == 0 {
		return nil
	}
	f, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, os.ModeAppend)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err != nil {
		return err
	}
	return nil
}

// hostEntries builds an /etc/hosts compliant text blob (as []byte]) for containers ipv4/6 address<->name pairs
func hostsEntries(containers []types.GenericContainer, bridgeName string) []byte {
	buff := bytes.Buffer{}
	for _, cont := range containers {
		if len(cont.Names) == 0 {
			continue
		}
		if cont.NetworkSettings.Set {
			if cont.NetworkSettings.IPv4addr != "" {
				buff.WriteString(cont.NetworkSettings.IPv4addr)
				buff.WriteString("\t")
				buff.WriteString(strings.TrimLeft(cont.Names[0], "/"))
				buff.WriteString("\n")
			}
			if cont.NetworkSettings.IPv6addr != "" {
				buff.WriteString(cont.NetworkSettings.IPv6addr)
				buff.WriteString("\t")
				buff.WriteString(strings.TrimLeft(cont.Names[0], "/"))
				buff.WriteString("\n")
			}
		}
	}
	return buff.Bytes()
}

func enrichNodes(containers []types.GenericContainer, nodes map[string]*types.NodeBase, mgmtNet string) {
	for _, c := range containers {
		name = c.Labels["clab-node-name"]
		if node, ok := nodes[name]; ok {
			// add network information
			// skipping host networking nodes as they don't have separate addresses
			if strings.ToLower(node.NetworkMode) == "host" {
				continue
			}

			if c.NetworkSettings.Set {
				node.MgmtIPv4Address = c.NetworkSettings.IPv4addr
				node.MgmtIPv4PrefixLength = c.NetworkSettings.IPv4pLen
				node.MgmtIPv6Address = c.NetworkSettings.IPv6addr
				node.MgmtIPv6PrefixLength = c.NetworkSettings.IPv6pLen
			}

			node.ContainerID = c.ID
		}

	}
}
