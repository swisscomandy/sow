package linux_container

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
        "net/http"

	"github.com/blang/semver"
	"github.com/cloudfoundry-incubator/garden"
	"github.com/cloudfoundry-incubator/garden-linux/linux_backend"
	"github.com/cloudfoundry-incubator/garden-linux/logging"
	"github.com/cloudfoundry-incubator/garden-linux/network"
	"github.com/cloudfoundry-incubator/garden-linux/network/subnets"
	"github.com/cloudfoundry-incubator/garden-linux/process_tracker"
	"github.com/cloudfoundry/gunk/command_runner"
	"github.com/pivotal-golang/lager"
)

var MissingVersion = semver.Version{}

type UndefinedPropertyError struct {
	Key string
}

func (err UndefinedPropertyError) Error() string {
	return fmt.Sprintf("property does not exist: %s", err.Key)
}

//go:generate counterfeiter -o fake_iptables_manager/fake_iptables_manager.go . IPTablesManager
type IPTablesManager interface {
	ContainerSetup(containerID, bridgeName string, ip net.IP, network *net.IPNet) error
	ContainerTeardown(containerID string) error
}

//go:generate counterfeiter -o fake_quota_manager/fake_quota_manager.go . QuotaManager
type QuotaManager interface {
	SetLimits(logger lager.Logger, containerRootFSPath string, limits garden.DiskLimits) error
	GetLimits(logger lager.Logger, containerRootFSPath string) (garden.DiskLimits, error)
	GetUsage(logger lager.Logger, containerRootFSPath string) (garden.ContainerDiskStat, error)

	Setup() error
}

//go:generate counterfeiter -o fake_network_statisticser/fake_network_statisticser.go . NetworkStatisticser
type NetworkStatisticser interface {
	Statistics() (stats garden.ContainerNetworkStat, err error)
}

//go:generate counterfeiter -o fake_watcher/fake_watcher.go . Watcher
type Watcher interface {
	Watch(func()) error
	Unwatch()
}

type BandwidthManager interface {
	SetLimits(lager.Logger, garden.BandwidthLimits) error
	GetLimits(lager.Logger) (garden.ContainerBandwidthStat, error)
}

type CgroupsManager interface {
	Set(subsystem, name, value string) error
	Get(subsystem, name string) (string, error)
	SubsystemPath(subsystem string) (string, error)
}

type LinuxContainer struct {
	propertiesMutex sync.RWMutex
	stateMutex      sync.RWMutex
	eventsMutex     sync.RWMutex
	bandwidthMutex  sync.RWMutex
	diskMutex       sync.RWMutex
	memoryMutex     sync.RWMutex
	cpuMutex        sync.RWMutex
	netInsMutex     sync.RWMutex
	netOutsMutex    sync.RWMutex
	graceTimeMutex  sync.RWMutex
	linux_backend.LinuxContainerSpec

	portPool         PortPool
	runner           command_runner.CommandRunner
	cgroupsManager   CgroupsManager
	quotaManager     QuotaManager
	bandwidthManager BandwidthManager
	processTracker   process_tracker.ProcessTracker
	filter           network.Filter
	ipTablesManager  IPTablesManager
	processIDPool    *ProcessIDPool

	graceTime time.Duration

	oomWatcher Watcher

	mtu uint32

	netStats NetworkStatisticser

	logger lager.Logger
}

type ProcessIDPool struct {
	currentProcessID uint32
	mu               sync.Mutex
}

func (p *ProcessIDPool) Next() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.currentProcessID = p.currentProcessID + 1
	return p.currentProcessID
}

func (p *ProcessIDPool) Restore(id uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if id >= p.currentProcessID {
		p.currentProcessID = id
	}
}

type PortPool interface {
	Acquire(int) (uint32, error)
	Remove(uint32) error
	Release(uint32)
}

func NewLinuxContainer(
	spec linux_backend.LinuxContainerSpec,
	portPool PortPool,
	runner command_runner.CommandRunner,
	cgroupsManager CgroupsManager,
	quotaManager QuotaManager,
	bandwidthManager BandwidthManager,
	processTracker process_tracker.ProcessTracker,
	filter network.Filter,
	ipTablesManager IPTablesManager,
	netStats NetworkStatisticser,
	oomWatcher Watcher,
	logger lager.Logger,
) *LinuxContainer {
	return &LinuxContainer{
		LinuxContainerSpec: spec,

		portPool:         portPool,
		runner:           runner,
		cgroupsManager:   cgroupsManager,
		quotaManager:     quotaManager,
		bandwidthManager: bandwidthManager,
		processTracker:   processTracker,
		filter:           filter,
		ipTablesManager:  ipTablesManager,
		processIDPool:    &ProcessIDPool{},
		netStats:         netStats,
		graceTime:        spec.GraceTime,

		oomWatcher: oomWatcher,
		logger:     logger,
	}
}

func (c *LinuxContainer) ID() string {
	return c.LinuxContainerSpec.ID
}

func (c *LinuxContainer) ResourceSpec() linux_backend.LinuxContainerSpec {
	return c.LinuxContainerSpec
}

func (c *LinuxContainer) RootFSPath() string {
	return c.ContainerRootFSPath
}

func (c *LinuxContainer) Handle() string {
	return c.LinuxContainerSpec.Handle
}

func (c *LinuxContainer) GraceTime() time.Duration {
	c.graceTimeMutex.RLock()
	defer c.graceTimeMutex.RUnlock()
	return c.graceTime
}

func (c *LinuxContainer) SetGraceTime(graceTime time.Duration) error {
	c.graceTimeMutex.Lock()
	defer c.graceTimeMutex.Unlock()
	c.graceTime = graceTime
	return nil
}

func (c *LinuxContainer) State() linux_backend.State {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()

	return c.LinuxContainerSpec.State
}

func (c *LinuxContainer) Events() []string {
	c.eventsMutex.RLock()
	defer c.eventsMutex.RUnlock()

	events := make([]string, len(c.LinuxContainerSpec.Events))
	copy(events, c.LinuxContainerSpec.Events)
	return events
}

func (c *LinuxContainer) Snapshot(out io.Writer) error {
	cLog := c.logger.Session("snapshot")

	cLog.Debug("saving")

	c.bandwidthMutex.RLock()
	defer c.bandwidthMutex.RUnlock()

	c.cpuMutex.RLock()
	defer c.cpuMutex.RUnlock()

	c.diskMutex.RLock()
	defer c.diskMutex.RUnlock()

	c.memoryMutex.RLock()
	defer c.memoryMutex.RUnlock()

	c.netInsMutex.RLock()
	defer c.netInsMutex.RUnlock()

	c.netOutsMutex.RLock()
	defer c.netOutsMutex.RUnlock()

	processSnapshots := []linux_backend.ActiveProcess{}

	for _, p := range c.processTracker.ActiveProcesses() {
		pid, err := strconv.Atoi(p.ID())
		if err != nil {
			panic(fmt.Sprintf("process id not a number: %s", p.ID())) // should never happen..
		}

		processSnapshots = append(processSnapshots, linux_backend.ActiveProcess{ID: uint32(pid)})
	}

	properties, _ := c.Properties()

	snapshot := ContainerSnapshot{
		ID:         c.ID(),
		Handle:     c.Handle(),
		RootFSPath: c.RootFSPath(),

		GraceTime: c.LinuxContainerSpec.GraceTime,

		State:  string(c.State()),
		Events: c.Events(),

		Limits: linux_backend.Limits{
			Bandwidth: c.LinuxContainerSpec.Limits.Bandwidth,
			CPU:       c.LinuxContainerSpec.Limits.CPU,
			Disk:      c.LinuxContainerSpec.Limits.Disk,
			Memory:    c.LinuxContainerSpec.Limits.Memory,
		},

		Resources: ResourcesSnapshot{
			RootUID: c.Resources.RootUID,
			Network: c.Resources.Network,
			Bridge:  c.Resources.Bridge,
			Ports:   c.Resources.Ports,
		},

		NetIns:  c.NetIns,
		NetOuts: c.NetOuts,

		Processes:               processSnapshots,
		DefaultProcessSignaller: true,

		Properties: properties,

		EnvVars: c.Env,
	}

	var err error

	err = json.NewEncoder(out).Encode(snapshot)
	if err != nil {
		cLog.Error("failed-to-save", err, lager.Data{
			"snapshot": snapshot,
		})
		return err
	}

	cLog.Info("saved", lager.Data{
		"snapshot": snapshot,
	})

	return nil
}

func (c *LinuxContainer) Restore(snapshot linux_backend.LinuxContainerSpec) error {
	cLog := c.logger.Session("restore")

	cLog.Debug("restoring")

	c.setState(linux_backend.State(snapshot.State))

	c.Env = snapshot.Env

	for _, ev := range snapshot.Events {
		c.registerEvent(ev)
	}

	if snapshot.Limits.Memory != nil {
		err := c.LimitMemory(*snapshot.Limits.Memory)
		if err != nil {
			cLog.Error("failed-to-limit-memory", err)
			return err
		}
	}

	signaller := c.processSignaller()

	for _, process := range snapshot.Processes {
		cLog.Info("restoring-process", lager.Data{
			"process": process,
		})

		c.processIDPool.Restore(process.ID)
		c.processTracker.Restore(fmt.Sprintf("%d", process.ID), signaller)
	}

	if err := c.ipTablesManager.ContainerSetup(snapshot.ID, snapshot.Resources.Bridge, snapshot.Resources.Network.IP, snapshot.Resources.Network.Subnet); err != nil {
		cLog.Error("failed-to-reenforce-network-rules", err)
		return err
	}

	for _, in := range snapshot.NetIns {
		if _, _, err := c.NetIn(in.HostPort, in.ContainerPort); err != nil {
			cLog.Error("failed-to-reenforce-port-mapping", err)
			return err
		}
	}

	for _, out := range snapshot.NetOuts {
		if err := c.NetOut(out); err != nil {
			cLog.Error("failed-to-reenforce-net-out", err)
			return err
		}
	}

	cLog.Info("restored")

	return nil
}

func (c *LinuxContainer) processSignaller() process_tracker.Signaller {
	var signaller process_tracker.Signaller

	// For backwards compatibility for pre 1.0.0 version containers
	if c.Version.Compare(MissingVersion) == 0 {
		signaller = &process_tracker.NamespacedSignaller{
			Logger:        c.logger,
			Runner:        c.runner,
			ContainerPath: c.ContainerPath,
			Timeout:       10 * time.Second,
		}
	} else {
		signaller = &process_tracker.LinkSignaller{}
	}

	return signaller
}

func (c *LinuxContainer) Start() error {
	cLog := c.logger.Session("start", lager.Data{"handle": c.Handle()})
	cLog.Debug("starting")

	cLog.Debug("iptables-setup-starting")
	err := c.ipTablesManager.ContainerSetup(
		c.ID(), c.Resources.Bridge, c.Resources.Network.IP, c.Resources.Network.Subnet,
	)
	if err != nil {
		cLog.Error("iptables-setup-failed", err)
		return fmt.Errorf("container: start: %v", err)
	}
	cLog.Debug("iptables-setup-ended")

	cLog.Debug("wshd-start-starting")
	start := exec.Command(path.Join(c.ContainerPath, "start.sh"))
	start.Env = []string{
		"id=" + c.ID(),
		"PATH=" + os.Getenv("PATH"),
	}

	cRunner := logging.Runner{
		CommandRunner: c.runner,
		Logger:        cLog,
	}

	err = cRunner.Run(start)
	if err != nil {
		cLog.Error("wshd-start-failed", err)
		return fmt.Errorf("container: start: %v", err)
	}
	cLog.Debug("wshd-start-ended")

	c.setState(linux_backend.StateActive)
	cLog.Debug("ended")
	return nil
}

func (c *LinuxContainer) Cleanup() error {
	cLog := c.logger.Session("cleanup")

	cLog.Debug("stopping-oom-notifier")
	c.oomWatcher.Unwatch()

	cLog.Info("done")
	return nil
}

func (c *LinuxContainer) Stop(kill bool) error {
	stop := exec.Command(path.Join(c.ContainerPath, "stop.sh"))
	if kill {
		stop.Args = append(stop.Args, "-w", "0")
	}

	if err := c.runner.Run(stop); err != nil {
		return err
	}

	if err := c.Cleanup(); err != nil {
		return err
	}

	c.setState(linux_backend.StateStopped)

	return nil
}

func (c *LinuxContainer) Properties() (garden.Properties, error) {
	c.propertiesMutex.RLock()
	defer c.propertiesMutex.RUnlock()

	return c.LinuxContainerSpec.Properties, nil
}

func (c *LinuxContainer) Property(key string) (string, error) {
	c.propertiesMutex.RLock()
	defer c.propertiesMutex.RUnlock()

	value, found := c.LinuxContainerSpec.Properties[key]
	if !found {
		return "", UndefinedPropertyError{key}
	}

	return value, nil
}

func (c *LinuxContainer) SetProperty(key string, value string) error {
	c.propertiesMutex.Lock()
	defer c.propertiesMutex.Unlock()

	props := garden.Properties{}
	for k, v := range c.LinuxContainerSpec.Properties {
		props[k] = v
	}

	props[key] = value

	c.LinuxContainerSpec.Properties = props

	return nil
}

func (c *LinuxContainer) RemoveProperty(key string) error {
	c.propertiesMutex.Lock()
	defer c.propertiesMutex.Unlock()

	if _, found := c.LinuxContainerSpec.Properties[key]; !found {
		return UndefinedPropertyError{key}
	}

	delete(c.LinuxContainerSpec.Properties, key)

	return nil
}

func (c *LinuxContainer) HasProperties(properties garden.Properties) bool {
	c.propertiesMutex.RLock()
	defer c.propertiesMutex.RUnlock()

	for k, v := range properties {
		if value, ok := c.LinuxContainerSpec.Properties[k]; !ok || (ok && value != v) {
			return false
		}
	}

	return true
}

func (c *LinuxContainer) Info() (garden.ContainerInfo, error) {
	c.logger.Debug("info-starting")

	mappedPorts := []garden.PortMapping{}

	c.netInsMutex.RLock()

	for _, spec := range c.NetIns {
		mappedPorts = append(mappedPorts, garden.PortMapping{
			HostPort:      spec.HostPort,
			ContainerPort: spec.ContainerPort,
		})
	}

	c.netInsMutex.RUnlock()

	var processIDs []string
	for _, process := range c.processTracker.ActiveProcesses() {
		processIDs = append(processIDs, process.ID())
	}

	properties, _ := c.Properties()

	info := garden.ContainerInfo{
		State:         string(c.State()),
		Events:        c.Events(),
		Properties:    properties,
		ContainerPath: c.ContainerPath,
		ProcessIDs:    processIDs,
		MappedPorts:   mappedPorts,
	}

	info.ContainerIP = c.Resources.Network.IP.String()
	info.HostIP = subnets.GatewayIP(c.Resources.Network.Subnet).String()
	info.ExternalIP = c.Resources.ExternalIP.String()

	c.logger.Debug("info-ended")

	return info, nil
}

func (c *LinuxContainer) StreamIn(spec garden.StreamInSpec) error {
	nsTarPath := path.Join(c.ContainerPath, "bin", "nstar")
	tarPath := path.Join(c.ContainerPath, "bin", "tar")
	pidPath := path.Join(c.ContainerPath, "run", "wshd.pid")

	pidFile, err := os.Open(pidPath)
	if err != nil {
		return err
	}

	var pid int
	_, err = fmt.Fscanf(pidFile, "%d", &pid)
	if err != nil {
		return err
	}

	user := spec.User
	if user == "" {
		user = "root"
	}

	buf := new(bytes.Buffer)
	tar := exec.Command(
		nsTarPath,
		tarPath,
		strconv.Itoa(pid),
		user,
		spec.Path,
	)
	tar.Stdout = buf
	tar.Stderr = buf

	tar.Stdin = spec.TarStream

	cLog := c.logger.Session("stream-in")

	cRunner := logging.Runner{
		CommandRunner: c.runner,
		Logger:        cLog,
	}

	if err := cRunner.Run(tar); err != nil {
		return fmt.Errorf("error streaming in: %v. Output: %s", err, buf.String())
	}
	return nil
}

func (c *LinuxContainer) StreamOut(spec garden.StreamOutSpec) (io.ReadCloser, error) {
	workingDir := filepath.Dir(spec.Path)
	compressArg := filepath.Base(spec.Path)
	if strings.HasSuffix(spec.Path, "/") {
		workingDir = spec.Path
		compressArg = "."
	}

	nsTarPath := path.Join(c.ContainerPath, "bin", "nstar")
	tarPath := path.Join(c.ContainerPath, "bin", "tar")
	pidPath := path.Join(c.ContainerPath, "run", "wshd.pid")

	pidFile, err := os.Open(pidPath)
	if err != nil {
		return nil, err
	}

	var pid int
	_, err = fmt.Fscanf(pidFile, "%d", &pid)
	if err != nil {
		return nil, err
	}

	user := spec.User
	if user == "" {
		user = "root"
	}

	tar := exec.Command(
		nsTarPath,
		tarPath,
		strconv.Itoa(pid),
		user,
		workingDir,
		compressArg,
	)

	tarRead, tarWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	tar.Stdout = tarWrite

	err = c.runner.Background(tar)
	if err != nil {
		return nil, err
	}

	// close our end of the tar pipe
	tarWrite.Close()

	go c.runner.Wait(tar)

	return tarRead, nil
}

func (c *LinuxContainer) NetIn(hostPort uint32, containerPort uint32) (uint32, uint32, error) {
        cLog := c.logger.Session("netin")

	cLog.Debug("Natting")
	if hostPort == 0 {
                space , _ := c.Property("network.space_id")
		randomPort, err := c.portPool.Acquire(GetPoolID(space))
		if err != nil {
			return 0, 0, err
		}
		c.Resources.AddPort(randomPort)

		hostPort = randomPort
	}
	if containerPort == 0 {
		containerPort = hostPort
	}
        if containerPort != 2222 {
           info, err1 := c.Info()
           space, err2 := c.Property("network.space_id")
           ip := info.ExternalIP
           if (err1 == nil &&  err2 == nil) {
              postendpoint(space,ip,hostPort)
           }
        }
	net := exec.Command(path.Join(c.ContainerPath, "net.sh"), "in")
	net.Env = []string{
		fmt.Sprintf("HOST_PORT=%d", hostPort),
		fmt.Sprintf("CONTAINER_PORT=%d", containerPort),
		"PATH=" + os.Getenv("PATH"),
	}

	err := c.runner.Run(net)
	if err != nil {
		return 0, 0, err
	}

	c.netInsMutex.Lock()
	defer c.netInsMutex.Unlock()

	c.NetIns = append(c.NetIns, linux_backend.NetInSpec{hostPort, containerPort})

	return hostPort, containerPort, nil
}

func postendpoint(space string, ip string, port uint32) {
	b :=  "{\"space\" : \"" + space + "\"," + "\"endpoint\" : \"" + ip + ":" + strconv.FormatUint(uint64(port), 10) + "\"}"
	response, _ := http.Post( GetUrl(),"application/json",bytes.NewBufferString(b))
	defer response.Body.Close()
}

func (c *LinuxContainer) NetOut(r garden.NetOutRule) error {
	err := c.filter.NetOut(r)
	if err != nil {
		return err
	}

	c.netOutsMutex.Lock()
	defer c.netOutsMutex.Unlock()

	c.NetOuts = append(c.NetOuts, r)

	return nil
}

func (c *LinuxContainer) setState(state linux_backend.State) {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	c.LinuxContainerSpec.State = state
}

func (c *LinuxContainer) registerEvent(event string) {
	c.eventsMutex.Lock()
	defer c.eventsMutex.Unlock()

	c.LinuxContainerSpec.Events = append(c.LinuxContainerSpec.Events, event)
}
