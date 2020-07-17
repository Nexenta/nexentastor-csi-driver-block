package driver

import (
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "strings"
    "strconv"
    "time"

    "github.com/container-storage-interface/spec/lib/go/csi"
    "github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
    "github.com/sirupsen/logrus"
    "golang.org/x/net/context"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
    "k8s.io/kubernetes/pkg/util/mount"

    "github.com/cenkalti/backoff"
    "github.com/Nexenta/go-nexentastor/pkg/ns"
    "github.com/Nexenta/nexentastor-csi-driver-block/pkg/config"
)

// NodeServer - k8s csi driver node server
type NodeServer struct {
    nodeID          string
    nsResolverMap   map[string]*ns.Resolver
    config          *config.Config
    log             *logrus.Entry
}

type CreateMappingParams struct {
    Address     string
    Target      string
    TargetGroup string
    Port        string
    VolumePath  string
    HostGroup   string
}

const (
    DefaultISCSIPort = "3260"
    DefaultHostGroup = "all"
)


func (s *NodeServer) refreshConfig(secret string) error {
    changed, err := s.config.Refresh(secret)
    if err != nil {
        return err
    }
    if changed {
        s.log.Info("config has been changed, updating...")
        for name, cfg := range s.config.NsMap {
            s.nsResolverMap[name], err = ns.NewResolver(ns.ResolverArgs{
                Address:            cfg.Address,
                Username:           cfg.Username,
                Password:           cfg.Password,
                Log:                s.log,
                InsecureSkipVerify: true, //TODO move to config
            })
            if err != nil {
                return fmt.Errorf("Cannot create NexentaStor resolver: %s", err)
            }
        }
    }
    return nil
}

// NodeGetInfo - get node info
func (s *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
    s.log.WithField("func", "NodeGetInfo()").Infof("request: '%+v'", req)

    return &csi.NodeGetInfoResponse{
        NodeId: s.nodeID,
        AccessibleTopology: &csi.Topology{
                Segments: map[string]string{},
        },
    }, nil
}

func (s *NodeServer) resolveNS(configName, volumeGroup string) (nsProvider ns.ProviderInterface, err error, name string) {
    l := s.log.WithField("func", "resolveNS()")
    l.Infof("configName: %+v, volumeGroup: %+v", configName, volumeGroup)
    resolver := s.nsResolverMap[configName]
    nsProvider, err = resolver.ResolveFromVg(volumeGroup)
    if err != nil {
        code := codes.Internal
        if ns.IsNotExistNefError(err) {
            code = codes.NotFound
        }
        return nil, status.Errorf(
            code,
            "Cannot resolve '%s' on any NexentaStor(s): %s",
            volumeGroup,
            err,
        ), ""
    }
    return nsProvider, nil, configName
}

// ISCSILogInRescan - Attempts login to iSCSI target, rescan if already logged.
func (s* NodeServer) ISCSILogInRescan(target, portal string) (error) {
    l := s.log.WithField("func", "ISCSILogInRescan()")
    cmd := exec.Command("iscsiadm", "-m", "discovery", "-t", "sendtargets", "-p", portal)
    l.Infof("Executing command: %+v", cmd)
    out, err := cmd.CombinedOutput()
    if err != nil {
        return err
    }
    cmd = exec.Command("iscsiadm", "-m", "node", "-T", target, "-p", portal, "-l")
    l.Infof("Executing command: %+v", cmd)
    out, err = cmd.CombinedOutput()
    if err != nil {
        if !strings.Contains(string(out), "already present") {
            return err
        } else {
            cmd := exec.Command("iscsiadm", "-m", "node", "-T", target, "-p", portal, "--rescan")
            l.Infof("Executing command: %+v", cmd)
            _, err = cmd.CombinedOutput()
            if err != nil {
                return err
            }
        }
    }
    return nil
}

// getRealDeviceName - get device name (e.g. /dev/sdb) from a symlink
func (s *NodeServer) GetRealDeviceName(symLink string) (string, error) {
    l := s.log.WithField("func", "GetRealDeviceName()")
    l.Infof("Evaluating symLink: %s", symLink)
    devName, err := filepath.EvalSymlinks(fmt.Sprintf("/host/%s", symLink))
    if err != nil {
        return "", err
    }
    devName = strings.TrimPrefix(devName, "/host")
    l.Infof("Device name is: %s", devName)
    return devName, err
}

// RemoveDevice - remove device (e.g. /dev/sdb) after deleting LUN
func (s *NodeServer) RemoveDevice(devName string) (error) {
    l := s.log.WithField("func", "RemoveDevice()")
    var (
        f   *os.File
        err error
    )
    filename := fmt.Sprintf("/host/sys/block%s/device/delete", strings.TrimPrefix(devName, "/dev"))
    if f, err = os.OpenFile(filename, os.O_APPEND | os.O_WRONLY, 0200); err != nil {
        l.Warnf("Could not open file %s for writing.", filename)
        return nil
    }

    l.Infof("Attempting write to file: %s", filename)
    if written, err := f.WriteString("1"); err != nil {
        l.Warnf("Could not write to file %s. Error: %+v", filename, err.Error())
        f.Close()
        return nil
    } else if written == 0 {
        l.Warnf("No data written to file %s.", filename)
        f.Close()
        return nil
    }

    l.Infof("Successfully deleted device: %s", devName)
    f.Close()
    return nil
}

// NodePublishVolume - mounts NS fs to the node
func (s *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (
    *csi.NodePublishVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "NodePublishVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))
    volumeContext := req.GetVolumeContext()
    volumeID := req.GetVolumeId()
    capability := req.GetVolumeCapability()
    if len(volumeID) == 0 {
        return nil, status.Error(codes.InvalidArgument, "req.VolumeId must be provided")
    }

    targetPath := req.GetTargetPath()
    if len(targetPath) == 0 {
        return nil, status.Error(codes.InvalidArgument, "req.TargetPath must be provided")
    }

    var secret string
    secrets := req.GetSecrets()
    for _, v := range secrets {
        secret = v
    }
    // read and validate config
    err := s.refreshConfig(secret)
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }

    splittedVol := strings.Split(volumeID, ":")
    if len(splittedVol) != 2 {
        return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeId is in wrong format: %s", volumeID))
    }
    configName, volumePath := splittedVol[0], splittedVol[1]
    cfg := s.config.NsMap[configName]
    nsProvider, err, configName := s.resolveNS(configName, cfg.DefaultVolumeGroup)
    if err != nil {
        return nil, err
    }
    
    hostGroup := volumeContext["HostGroup"]
    if hostGroup == "" {
        if cfg.DefaultHostGroup != "" {
            hostGroup = cfg.DefaultHostGroup
        } else {
            hostGroup = DefaultHostGroup
        }
    }
    targetGroup := volumeContext["TargetGroup"]
    if targetGroup == "" {
        targetGroup = cfg.DefaultTargetGroup
    }
    port := volumeContext["iSCSIPort"]
    if port == "" {
        if cfg.DefaultISCSIPort != "" {
            port = cfg.DefaultISCSIPort
        } else {
            port = DefaultISCSIPort
        }
    }
    dataIP := volumeContext["dataIP"]
    if dataIP == "" {
        dataIP = cfg.DefaultDataIP
    }
    target := volumeContext["Target"]
    if target == "" {
        target = cfg.DefaultTarget
    }
    portal := fmt.Sprintf("%s:%s", dataIP, port)

    params := CreateMappingParams{
        Address: dataIP,
        Target: target,
        TargetGroup: targetGroup,
        Port: port,
        VolumePath: volumePath,
        HostGroup: hostGroup,
    }
    err = s.CreateISCSIMapping(params, nsProvider)
    if err != nil {
        return nil, err
    }

    err = s.ISCSILogInRescan(target, portal)
    if err != nil {
        return nil, err
    }

    getLunResp, err := nsProvider.GetLunMapping(volumePath)
    if err != nil{
        return nil, err
    }
    lunNumber := getLunResp.Lun
    // Make dir if dir not present
    _, err = os.Stat(targetPath)
    if os.IsNotExist(err) {
        if err = os.MkdirAll(filepath.Dir(targetPath), 0750); err != nil {
            return nil, status.Error(codes.Internal, err.Error())
        }
    }
    devByPath := strings.Join([]string{
        "/dev/disk/by-path/ip", portal,
        "iscsi", target, "lun", strconv.Itoa(lunNumber)}, "-")
    // check if device is visible, wait if not
    found := false
    sleepTime := 100 * time.Millisecond
    for ok := true; ok; ok = found {
        if _, err := os.Stat(devByPath); os.IsNotExist(err) {
            l.Infof("Device %s not found, sleep %s", devByPath, sleepTime)
            time.Sleep(sleepTime)
        } else {
            found = true
        }
    }
    devName, err := s.GetRealDeviceName(devByPath)
    if err != nil {
        return nil, err
    }

    if capability.GetMount() != nil {
        fsType := capability.GetMount().GetFsType()
        deviceFS := s.getFSType(devName)
        if deviceFS == "" {
            if fsType == "" {
                fsType = "ext4"
            }
            err = s.formatVolume(devName, fsType)
            if err != nil {
                return nil, err
            }
        } else if deviceFS != fsType {
            return nil, fmt.Errorf(
                "Volume %s is already formatted in %s, requested: %s,", volumeID, deviceFS, fsType)
        }

        err = s.mountVolume(devName, targetPath, fsType, req)
        if err != nil {
            return nil, err
        }
    } else {
        cmd := exec.Command("ln", "-s", devName, targetPath)
        l.Infof("Executing command: %+v", cmd)
        _, err = cmd.CombinedOutput()
        if err != nil {
            return nil, err
        }
    }
    l.Infof("Device %s mounted to %s", devName, targetPath)
    return &csi.NodePublishVolumeResponse{}, nil
}

func (s *NodeServer) CreateISCSIMapping(params CreateMappingParams, nsProvider ns.ProviderInterface) error {
    l := s.log.WithField("func", "CreateISCSIMapping()")
    l.Infof("Creating iSCSI mapping with params: %+v", params)

    iSCSIParams := ns.CreateISCSITargetParams{
        Name: params.Target,
    }
    portInt, err := strconv.Atoi(params.Port)
    if err != nil {
        l.Errorf("Could not convert port to int, port: %s, err: %s", params.Port, err.Error())
        return err
    }
    iSCSIParams.Portals = append(iSCSIParams.Portals, ns.Portal{
        Address: params.Address,
        Port: portInt,
    })
    err = nsProvider.CreateISCSITarget(iSCSIParams)
    if err != nil {
        return err
    }

    createTargetGroupParams := ns.CreateTargetGroupParams{
        Name: params.TargetGroup,
        Members: []string{params.Target},
    }
    err = nsProvider.CreateUpdateTargetGroup(createTargetGroupParams)
    if err != nil {
        return err
    }

    err = nsProvider.CreateLunMapping(ns.CreateLunMappingParams{
        Volume: params.VolumePath,
        TargetGroup: params.TargetGroup,
        HostGroup: params.HostGroup,
    })
    if err != nil {
        return err
    }
    return nil
}

func (s *NodeServer) mountVolume(devName, targetPath, fsType string, req *csi.NodePublishVolumeRequest) error {
    l := s.log.WithField("func", "mountVolume()")
    l.Infof("Mounting device %s to targetPath %s", devName, targetPath)

    mounter := mount.New("")
    notMnt, err := mounter.IsLikelyNotMountPoint(targetPath)
    if err != nil {
        if os.IsNotExist(err) {
            if err := os.MkdirAll(targetPath, 0750); err != nil {
                l.Errorf("Failed to mkdir to target path %s. Error: %s", targetPath, err)
                return status.Error(codes.Internal, err.Error())
            }
            notMnt = true
        } else {
            l.Errorf("Failed to mkdir to target path %s. Error: %s", targetPath, err)
            return status.Error(codes.Internal, err.Error())
        }
    }

    if !notMnt {
        l.Warningf("Skipped mount volume %s. Error: %s", targetPath, err)
        return nil
    }

    readOnly := req.GetReadonly()
    mountOptions := req.GetVolumeCapability().GetMount().GetMountFlags()

    if readOnly {
        if !stringInArray(mountOptions, "ro") {
            mountOptions = append(mountOptions, "ro")
        }
    }

    l.Infof("target %v, fstype %v, readonly %v, mountOptions %v", targetPath, fsType, readOnly, mountOptions)
    err = mounter.Mount(fmt.Sprintf("/host%s", devName), targetPath, fsType, []string{})
    if err != nil {
        if os.IsPermission(err) {
            l.Errorf("Failed to mount device %s. Error: %v", devName, err)
            return status.Error(codes.PermissionDenied, err.Error())
        }
        if strings.Contains(err.Error(), "invalid argument") {
            l.Errorf("Failed to mount device %s. Error: %v", devName, err)
            return status.Error(codes.InvalidArgument, err.Error())
        }
        l.Errorf("Failed to mount device %+v. Error: %v", devName, err)
        return status.Error(codes.Internal, err.Error())
    }
    return nil
}

// formatVolume creates a filesystem for the supplied device of the supplied type.
func (s *NodeServer) formatVolume(device, fstype string) error {
    l := s.log.WithField("func", "formatVolume()")

    start := time.Now()
    maxDuration := 30 * time.Second

    formatVolume := func() error {

        var err error
        l.Infof("Trying to format %s via %s", device, fstype)
        switch fstype {
        case "xfs":
            cmd := exec.Command("mkfs.xfs", "-K", "-f", device)
            l.Infof("Executing command: %+v", cmd)
            _, err = cmd.CombinedOutput()
        case "ext3":
            cmd := exec.Command("mkfs.ext3", "-E", "nodiscard", "-F", device)
            l.Infof("Executing command: %+v", cmd)
            _, err = cmd.CombinedOutput()
        case "ext4":
            cmd := exec.Command("mkfs.ext4", "-E", "nodiscard", "-F", device)
            l.Infof("Executing command: %+v", cmd)
            _, err = cmd.CombinedOutput()
        default:
            return fmt.Errorf("unsupported file system type: %s", fstype)
        }

        if err != nil {
            l.Errorf("Formating error %s", err)
        }
        return err
    }

    formatNotify := func(err error, duration time.Duration) {
        l.Info("Format failed, retrying. Duration: %v", duration)
    }

    formatBackoff := backoff.NewExponentialBackOff()
    formatBackoff.InitialInterval = 2 * time.Second
    formatBackoff.Multiplier = 2
    formatBackoff.RandomizationFactor = 0.1
    formatBackoff.MaxElapsedTime = maxDuration

    // Run the check/rescan using an exponential backoff
    if err := backoff.RetryNotify(formatVolume, formatBackoff, formatNotify); err != nil {
        l.Infof("Could not format device after %3.2f seconds.", maxDuration.Seconds())
        return err
    }
    elapsed := time.Since(start)
    l.Infof("Device formatted in %s", elapsed)
    return nil
}

// NodeUnpublishVolume - umount NS fs from the node and delete directory if successful
func (s *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (
    *csi.NodeUnpublishVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "NodeUnpublishVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    volumeID := req.GetVolumeId()
    if len(volumeID) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Volume ID must be provided")
    }

    targetPath := req.GetTargetPath()
    if len(targetPath) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Target path must be provided")
    }

    splittedVol := strings.Split(volumeID, ":")
    if len(splittedVol) != 2 {
        return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeId is in wrong format: %s", volumeID))
    }
    configName, volumePath := splittedVol[0], splittedVol[1]
    cfg := s.config.NsMap[configName]
    nsProvider, err, configName := s.resolveNS(configName, cfg.DefaultVolumeGroup)
    if err != nil {
        return nil, err
    }

    getLunResp, err := nsProvider.GetLunMapping(volumePath)
    if err != nil{
        if !ns.IsNotExistNefError(err) {
            return nil, err
        } else {
            l.Infof("Lun mapping %s for volume %s not found, that's OK for deletion", getLunResp.Id, volumePath)
            return &csi.NodeUnpublishVolumeResponse{}, nil
        }
    } else {
        err = nsProvider.DestroyLunMapping(getLunResp.Id)
        if err != nil{
            return nil, err
        }
    }

    port := cfg.DefaultISCSIPort
    if port == "" {
        port = DefaultISCSIPort
    }
    dataIP := cfg.DefaultDataIP
    portal := fmt.Sprintf("%s:%s", dataIP, port)
    target := cfg.DefaultTarget
    lunNumber := getLunResp.Lun
    devByPath := strings.Join([]string{
        "/dev/disk/by-path/ip", portal,
        "iscsi", target, "lun", strconv.Itoa(lunNumber)}, "-")
    dev, err := s.GetRealDeviceName(devByPath)
    if err != nil {
        return nil, err
    }

    err = s.RemoveDevice(dev)
    if err != nil {
        return nil, err
    }
    targetPath = req.GetTargetPath()
    if len(targetPath) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Target path must be provided")
    }

    mounter := mount.New("")

    notMountPoint, err := mounter.IsLikelyNotMountPoint(targetPath)
    if err != nil {
        if os.IsNotExist(err) {
            l.Warnf("mount point '%s' already doesn't exist: '%s', return OK", targetPath, err)
            return &csi.NodeUnpublishVolumeResponse{}, nil
        }
        return nil, status.Errorf(
            codes.Internal,
            "Cannot ensure that target path '%s' is a mount point: '%s'",
            targetPath,
            err,
        )
    }

    if notMountPoint {
        if err := os.Remove(targetPath); err != nil {
            l.Infof("Remove target path error: %s", err.Error())
        }
        return &csi.NodeUnpublishVolumeResponse{}, nil
    }

    if err := mounter.Unmount(targetPath); err != nil {
        return nil, status.Errorf(codes.Internal, "Failed to unmount target path '%s': %s", targetPath, err)
    }

    if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
        return nil, status.Errorf(codes.Internal, "Cannot remove unmounted target path '%s': %s", targetPath, err)
    }

    return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetCapabilities - get node capabilities
func (s *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (
    *csi.NodeGetCapabilitiesResponse,
    error,
) {
    s.log.WithField("func", "NodeGetCapabilities()").Infof("request: '%+v'", req)

    return &csi.NodeGetCapabilitiesResponse{
        Capabilities: []*csi.NodeServiceCapability{},
    }, nil
}

// NodeGetVolumeStats - volume stats (available capacity)
func (s *NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (
    *csi.NodeGetVolumeStatsResponse,
    error,
) {
    l := s.log.WithField("func", "NodeGetVolumeStats()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    // volumePath can be any valid path where volume was previously staged or published.
    // It MUST be an absolute path in the root filesystem of the process serving this request.
    //TODO validate volumePath then re-enable GET_VOLUME_STATS node capability.
    volumePath := req.GetVolumePath()
    if len(volumePath) == 0 {
        return nil, status.Error(codes.InvalidArgument, "req.VolumePath must be provided")
    }

    volumeID := req.GetVolumeId()
    if len(volumeID) == 0 {
        return nil, status.Error(codes.InvalidArgument, "req.VolumeId must be provided")
    }
    // read and validate config
    err := s.refreshConfig("")
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }

    splittedVol := strings.Split(volumeID, ":")
    if len(splittedVol) != 2 {
        return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeId is in wrong format: %s", volumeID))
    }
    configName, volumePath := splittedVol[0], splittedVol[1]
    nsProvider, err, _ := s.resolveNS(configName, volumePath)
    if err != nil {
        return nil, err
    }

    l.Infof("resolved NS: %s, %s", nsProvider, volumePath)

    // get NexentaStor filesystem information
    available, err := nsProvider.GetFilesystemAvailableCapacity(volumePath)
    if err != nil {
        return nil, status.Errorf(codes.NotFound, "Cannot find filesystem '%s': %s", volumeID, err)
    }

    return &csi.NodeGetVolumeStatsResponse{
        Usage: []*csi.VolumeUsage{
            {
                Unit:      csi.VolumeUsage_BYTES,
                Available: available,
                //TODO add used, total
            },
        },
    }, nil
}

// NodeStageVolume - stage volume
//TODO use this to mount NFS, then do bind mount?
func (s *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (
    *csi.NodeStageVolumeResponse,
    error,
) {
    s.log.WithField("func", "NodeStageVolume()").Warnf("request: %+v", req)

    return nil, status.Error(codes.Unimplemented, "")
}

// NodeUnstageVolume - unstage volume
//TODO use this to umount NFS?
func (s *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (
    *csi.NodeUnstageVolumeResponse,
    error,
) {
    s.log.WithField("func", "NodeUnstageVolume()").Warnf("request: '%+v' - not implemented", req)
    return nil, status.Error(codes.Unimplemented, "")
}

// NodeExpandVolume - not supported
func (s *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (
    *csi.NodeExpandVolumeResponse,
    error,
) {
    s.log.WithField("func", "NodeExpandVolume()").Warnf("request: '%+v' - not implemented", req)
    return nil, status.Error(codes.Unimplemented, "")
}

// NewNodeServer - create an instance of node service
func NewNodeServer(driver *Driver) (*NodeServer, error) {
    l := driver.log.WithField("cmp", "NodeServer")
    l.Info("create new NodeServer...")
    resolverMap := make(map[string]*ns.Resolver)

    for name, cfg := range driver.config.NsMap {
        nsResolver, err := ns.NewResolver(ns.ResolverArgs{
            Address:            cfg.Address,
            Username:           cfg.Username,
            Password:           cfg.Password,
            Log:                l,
            InsecureSkipVerify: true, //TODO move to config
        })
        if err != nil {
            return nil, fmt.Errorf("Cannot create NexentaStor resolver: %s", err)
        }
        resolverMap[name] = nsResolver
    }

    return &NodeServer{
        nodeID:         driver.nodeID,
        nsResolverMap:  resolverMap,
        config:         driver.config,
        log:            l,
    }, nil
}

func stringInArray(arr []string, tofind string) bool {
    for _, item := range arr {
        if item == tofind {
            return true
        }
    }
    return false
}

// getFSType returns the filesystem for the supplied devName.
func (s *NodeServer) getFSType(devName string) string {
    l := s.log.WithField("func", "getFSType()")
    cmd := exec.Command("blkid", devName)
    l.Infof("Executing command: %+v", cmd)
    out, err := cmd.CombinedOutput()
    fsType := ""
    if err != nil {
        l.Infof("Could not get FSType for device.")
        return fsType
    }

    if strings.Contains(string(out), "TYPE=") {
        for _, v := range strings.Split(string(out), " ") {
            if strings.Contains(v, "TYPE=") {
                fsType = strings.Split(v, "=")[1]
                fsType = strings.Replace(fsType, "\"", "", -1)
                fsType = strings.TrimSpace(fsType)
            }
        }
    }
    return fsType
}
