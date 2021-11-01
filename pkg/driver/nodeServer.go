package driver

import (
    "fmt"
    "io/ioutil"
    "os"
    "os/exec"
    "path/filepath"
    "strings"
    "strconv"
    "time"

    "github.com/container-storage-interface/spec/lib/go/csi"
    "github.com/google/uuid"
    "github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
    "github.com/sirupsen/logrus"
    "golang.org/x/net/context"
    "golang.org/x/sys/unix"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
    "k8s.io/utils/mount"
    "k8s.io/kubernetes/pkg/util/resizefs"
    utilexec "k8s.io/utils/exec"

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
    TargetGroup string
    VolumePath  string
    HostGroup   string
}

type CreateTargetTgParams struct {
    Address             string
    Port                string
    Target              string
    ISCSITargetPrefix   string
    TargetGroup         string
    NumOfLunsPerTarget  int
}

const (
    DefaultISCSITargetPrefix = "iqn.2005-07.com.nexenta"
    DefaultFsType = "ext4"
    DefaultISCSIPort = "3260"
    HostGroupPrefix = "csi"
    PathToInitiatorName = "/host/etc/iscsi/initiatorname.iscsi"
    DefaultDynamicTargetLunAllocation = true
    DefaultNumOfLunsPerTarget = 256
    DefaultUseChapAuth = false
    DefaultMountPointPermissions = 0750
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
    l.Debugf("Executing command: %+v", cmd)
    out, err := cmd.CombinedOutput()
    if err != nil {
        l.Errorf("iscsiadm discovery error: %+v", err)
        return err
    }
    cmd = exec.Command("iscsiadm", "-m", "node", "-T", target, "-p", portal, "-l")
    l.Debugf("Executing command: %+v", cmd)
    out, err = cmd.CombinedOutput()
    if err != nil {
        if !strings.Contains(string(out), "already present") {
            return status.Errorf(codes.Unauthenticated, "Was not able to login to target, err: %+v", err)
            // return err
        } else {
            cmd := exec.Command("iscsiadm", "-m", "node", "-T", target, "-p", portal, "--rescan")
            l.Debugf("Executing command: %+v", cmd)
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
    l.Debugf("Evaluating symLink: %s", symLink)
    devName, err := filepath.EvalSymlinks(fmt.Sprintf("/host/%s", symLink))
    if err != nil {
        l.Errorf("Could not evaluate symlink: %s, err: %+v", symLink, err)
        return "", err
    }
    if strings.HasPrefix(devName, "/host") {
        devName = strings.TrimPrefix(devName, "/host")
    }
    l.Debugf("Device name is: %s", devName)
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
        l.Warnf("Could not open file %s for writing, err: %+v", filename, err)
        return nil
    }

    l.Debugf("Attempting write to file: %s", filename)
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

func (s *NodeServer) RescanDevice(devName string) (error) {
    l := s.log.WithField("func", "RescanDevice()")
    var (
        f   *os.File
        err error
    )
    filename := fmt.Sprintf("/sys/block%s/device/rescan", strings.TrimPrefix(devName, "/dev"))
    if f, err = os.OpenFile(filename, os.O_APPEND | os.O_WRONLY, 0200); err != nil {
        l.Warnf("Could not open file %s for writing, err: %+v", filename, err)
        return nil
    }

    l.Debugf("Attempting write to file: %s", filename)
    if written, err := f.WriteString("1"); err != nil {
        l.Warnf("Could not write to file %s. Error: %+v", filename, err.Error())
        f.Close()
        return nil
    } else if written == 0 {
        l.Warnf("No data written to file %s.", filename)
        f.Close()
        return nil
    }

    l.Debugf("Successfully rescanned device: %s", devName)
    f.Close()
    return nil
}

// ResolveTargetGroup - find target with lowest lunmappings or create new one
func (s *NodeServer) ResolveTargetGroup(params CreateTargetTgParams, nsProvider ns.ProviderInterface) (
    target, targetGroup string,
    err error,
) {
    l := s.log.WithField("func", "ResolveTargetGroup()")
    l.Infof("numOfLunsPerTarget: %+v, iSCSITargetPrefix: %+v", params.NumOfLunsPerTarget, params.ISCSITargetPrefix)
    targetGroups, err := nsProvider.GetTargetGroups()
    if err != nil {
        return target, targetGroup, nil
    }

    var minLuns int
    var minTargetGroup string
    for _, currentTg := range targetGroups {
        for _, currentTarget := range currentTg.Members {
            if strings.HasPrefix(currentTarget, params.ISCSITargetPrefix) {
                lunMappingParams := ns.GetLunMappingsParams{
                    TargetGroup: currentTg.Name,
                }
                luns, err := nsProvider.GetLunMappings(lunMappingParams)
                if err != nil {
                    return target, targetGroup, nil
                }
                if (minLuns == 0 || len(luns) < minLuns) && len(luns) < params.NumOfLunsPerTarget {
                    minLuns = len(luns)
                    minTargetGroup = currentTg.Name
                    target = currentTarget
                }
            }
        }
    }
    if minTargetGroup != "" {
        return target, minTargetGroup, err
    } else {
        return s.CreateNewTargetTg(params, nsProvider)
    }
}

func (s *NodeServer) CreateNewTargetTg(params CreateTargetTgParams, nsProvider ns.ProviderInterface) (
    target, targetGroup string,
    err error,
) {
    l := s.log.WithField("func", "CreateNewTargetTg()")
    l.Infof("params: '%+v'", params)
    if params.Target == "" {
        targetGroup = uuid.New().String()
        target = fmt.Sprintf("%s:%s", params.ISCSITargetPrefix, targetGroup)
    } else {
        target = params.Target
        if params.TargetGroup == "" {
            splittedTarget := strings.Split(target, ":")
            targetGroup = splittedTarget[len(splittedTarget) - 1]
        } else {
            targetGroup = params.TargetGroup
        }
    }
    portInt, err := strconv.Atoi(params.Port)
    if err != nil {
        l.Errorf("Could not convert port to int, port: %s, err: %s", params.Port, err.Error())
        return target, targetGroup, err
    }
    portal := ns.Portal{
        Address: params.Address,
        Port: portInt,
    }
    createTargetParams := ns.CreateISCSITargetParams{
        Name: target,
        Portals: []ns.Portal{portal},
    }

    err = nsProvider.CreateISCSITarget(createTargetParams)
    if err != nil {
        return target, targetGroup, err
    }

    createTargetGroupParams := ns.CreateTargetGroupParams{
        Name: targetGroup,
        Members: []string{target},
    }
    err = nsProvider.CreateUpdateTargetGroup(createTargetGroupParams)
    if err != nil {
    }
    return target, targetGroup, err
}

func (s *NodeServer) ParseVolumeContext(
    volumeContext map[string]string, nsProvider ns.ProviderInterface, configName string) (
    hostGroup, targetGroup, port, dataIP, iSCSITarget string,
    err error,
) {
    l := s.log.WithField("func", "ParseVolumeContext()")
    cfg := s.config.NsMap[configName]
    targetGroup = volumeContext["TargetGroup"]
    iSCSITarget = volumeContext["Target"]
    iSCSITargetPrefix := cfg.ISCSITargetPrefix

    port = volumeContext["iSCSIPort"]
    if port == "" {
        if cfg.DefaultISCSIPort != "" {
            port = cfg.DefaultISCSIPort
        } else {
            port = DefaultISCSIPort
        }
    }
    if iSCSITargetPrefix == "" {
        iSCSITargetPrefix = DefaultISCSITargetPrefix
    }

    hostGroup = volumeContext["HostGroup"]
    if hostGroup == "" {
        if cfg.DefaultHostGroup != "" {
            hostGroup = cfg.DefaultHostGroup
        } else {
            hostGroup, err = s.CreateUpdateHostGroup(nsProvider)
            if err != nil {
                return hostGroup, targetGroup, port, dataIP, iSCSITarget, err
            }
        }
    }

    dataIP = volumeContext["dataIP"]
    if dataIP == "" {
        dataIP = cfg.DefaultDataIP
    }
    numOfLunsPerTarget, err := strconv.Atoi(volumeContext["numOfLunsPerTarget"])
    if err != nil {
        l.Debugf("Could not parse numOfLunsPerTarget, setting default: %+v", DefaultNumOfLunsPerTarget)
        numOfLunsPerTarget = DefaultNumOfLunsPerTarget
    }

    params := CreateTargetTgParams{
        Address: dataIP,
        Port: port,
        Target: iSCSITarget,
        ISCSITargetPrefix: iSCSITargetPrefix,
        NumOfLunsPerTarget: numOfLunsPerTarget,
        TargetGroup: targetGroup,
    }

    dynamicTargetLunAllocation, err := strconv.ParseBool(volumeContext["dynamicTargetLunAllocation"])
    if err != nil {
        l.Infof("Could not parse dynamicTargetLunAllocation, defaulting to %+v. Error: %+v", DefaultDynamicTargetLunAllocation, err)
        dynamicTargetLunAllocation = DefaultDynamicTargetLunAllocation
    }
    if dynamicTargetLunAllocation == true {
        iSCSITarget, targetGroup, err = s.ResolveTargetGroup(params, nsProvider)
        if err != nil {
            return hostGroup, targetGroup, port, dataIP, iSCSITarget, err
        }
    } else {
        iSCSITarget, targetGroup, err = s.CreateNewTargetTg(params, nsProvider)
    }

    // Check if CHAP auth is enabled
    useChapAuth, err := strconv.ParseBool(volumeContext["useChapAuth"])
    if err != nil {
        l.Debugf("Could not parse useChapAuth, defaulting to %+v. Error: %+v", DefaultUseChapAuth, err)
        useChapAuth = DefaultUseChapAuth
    }
    if useChapAuth == true {
        nodeIQN, err := s.GetNodeIQN()
        if err != nil {
            return hostGroup, targetGroup, port, dataIP, iSCSITarget, err
        }
        err = s.SetChapAuth(nodeIQN, volumeContext["chapUser"], volumeContext["chapSecret"], nsProvider)
        if err != nil {
            return hostGroup, targetGroup, port, dataIP, iSCSITarget, err
        }
        // Set authentication to CHAP for iSCSI target
        updateParams := ns.UpdateISCSITargetParams{
            Authentication: "chap",
        }
        err = nsProvider.UpdateISCSITarget(iSCSITarget, updateParams)
        if err != nil {
            return hostGroup, targetGroup, port, dataIP, iSCSITarget, err
        }
    }
    return hostGroup, targetGroup, port, dataIP, iSCSITarget, nil
}

func (s *NodeServer) SetChapAuth(name, chapUser, chapSecret string, nsProvider ns.ProviderInterface) (err error) {
    l := s.log.WithField("func", "SetChapAuth()")
    l.Warnf("params: name: %+v, chapUser: %+v, chapSecret: %+v", name, chapUser, chapSecret)
    if name == "" {
        return status.Error(codes.InvalidArgument, "iSCSI IQN not provided")
    }
    if chapSecret == "" {
        return status.Error(codes.InvalidArgument, "chapSecret not provided")
    }

    _, err = nsProvider.GetRemoteInitiator(name)
    if err != nil {
        if ns.IsNotExistNefError(err) {
            // Create new remote initiator
            createParams := ns.CreateRemoteInitiatorParams{
                Name: name,
                ChapUser: chapUser,
                ChapSecret: chapSecret,
            }
            err = nsProvider.CreateRemoteInitiator(createParams)
            if err != nil {
                return err
            }
            return nil
        } else {
            // Other error -> fail
            return err
        }
    }
    // No error means that remoteInitiator exists -> update with our credentials
    updateParams := ns.UpdateRemoteInitiatorParams{
        ChapUser: chapUser,
        ChapSecret: chapSecret,
    }
    err = nsProvider.UpdateRemoteInitiator(name, updateParams)
    if err != nil {
        return err
    }
    return nil
}

func (s *NodeServer) ConstructDevByPath(portal, iSCSITarget string, lunNumber int) (devByPath string) {
    strLun := ""
    if lunNumber > 255 {
        strLun = strings.TrimSuffix(fmt.Sprintf("0x%04x%013d", lunNumber, 1), "1")
    } else {
        strLun = strconv.Itoa(lunNumber)
    }
    return strings.Join([]string{
        "/dev/disk/by-path/ip", portal,
        "iscsi", iSCSITarget, "lun", strLun}, "-")
}

// NodeStageVolume - stage volume
func (s *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (
    *csi.NodeStageVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "NodeStageVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))
    volumeContext := req.GetVolumeContext()

    volumeID := req.GetVolumeId()
    if len(volumeID) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
    }

    targetPath := req.GetStagingTargetPath()
    if len(targetPath) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Staging targetPath not provided")
    }

    volumeCapability := req.GetVolumeCapability()
    if volumeCapability == nil {
        return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
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

    hostGroup, targetGroup, port, dataIP, iSCSITarget, err := s.ParseVolumeContext(
        volumeContext, nsProvider, configName)
    if err != nil {
        return nil, err
    }

    // Check if mapping already exists
    getLunParams := ns.GetLunMappingsParams{
        TargetGroup: targetGroup,
        Volume: volumePath,
        HostGroup: hostGroup,
    }
    lunMappings, err := nsProvider.GetLunMappings(getLunParams)
    if err != nil {
        return nil, err
    }

    if len(lunMappings) == 0 {
        params := CreateMappingParams{
            TargetGroup: targetGroup,
            VolumePath: volumePath,
            HostGroup: hostGroup,
        }
        err = s.CreateISCSIMapping(params, nsProvider)
        if err != nil {
            return nil, err
        }
        lunMappings, err = nsProvider.GetLunMappings(getLunParams)
        if err != nil {
            return nil, err
        }
    }
    lunNumber := lunMappings[0].Lun

    device := ""
    permissions, err := s.GetMountPointPermissions(volumeContext)
    if err != nil {
        return nil, err
    }
    _, err = os.Stat(targetPath)
    if os.IsNotExist(err) {
        if err = os.MkdirAll(filepath.Dir(targetPath), permissions); err != nil {
            return nil, status.Error(codes.Internal, err.Error())
        }
    } else {
        err = os.Chmod(targetPath, permissions)
        if err != nil {
            return nil, err
        }
        device, err = s.DeviceFromTargetPath(targetPath)
        if err != nil {
            device = ""
        }
    }
    portal := fmt.Sprintf("%s:%s", dataIP, port)
    err = s.ISCSILogInRescan(iSCSITarget, portal)
    if err != nil {
        return nil, err
    }

    devByPath := s.ConstructDevByPath(portal, iSCSITarget, lunNumber)
    found := false
    sleepTime := 500 * time.Millisecond
    timeout := 60 * time.Second
    for !found {
        if sleepTime > timeout {
            return nil, status.Error(codes.DeadlineExceeded, "Mount is nil in volume capability")
        }
        if _, err := os.Stat(filepath.Join("/host", devByPath)); os.IsNotExist(err) {
            l.Infof("Device %s not found, sleep %s", devByPath, sleepTime)
            time.Sleep(sleepTime)
            sleepTime *= 2
        } else {
            l.Infof("Device %s found", devByPath)
            found = true
        }
    }
    source, err := s.GetRealDeviceName(devByPath)
    if err != nil {
        return nil, err
    }

    // This operation (NodeStageVolume) MUST be idempotent.
    // If the volume corresponding to the volume_id is already staged to the staging_target_path,
    // and is identical to the specified volume_capability the Plugin MUST reply 0 OK.
    if device == source {
        l.Infof("Volume=%q already staged", volumeID)
        return &csi.NodeStageVolumeResponse{}, nil
    }

    switch volumeCapability.GetAccessType().(type) {
    case *csi.VolumeCapability_Block:
        targetPath = filepath.Join(targetPath, "device")
        cmd := exec.Command("ln", "-s", source, targetPath)
        l.Debugf("Executing command: %+v", cmd)
        _, err = cmd.CombinedOutput()
        if err != nil {
            return nil, err
        }
        return &csi.NodeStageVolumeResponse{}, nil
    }

    capabilityMount := volumeCapability.GetMount()
    if capabilityMount == nil {
        return nil, status.Error(codes.InvalidArgument, "Mount is nil in volume capability")
    }

    fsType := capabilityMount.GetFsType()
    if len(fsType) == 0 {
        fsType = DefaultFsType
    }
    deviceFS := s.getFSType(source)
    if deviceFS == "" {
        if fsType == "" {
            fsType = DefaultFsType
        }
        err = s.formatVolume(source, fsType)
        if err != nil {
            return nil, err
        }
    } else if deviceFS != fsType {
        return nil, fmt.Errorf(
            "Volume %s is already formatted in %s, requested: %s,", volumeID, deviceFS, fsType)
    }

    var mountOptions []string
    for _, f := range capabilityMount.MountFlags {
        mountOptions = append(mountOptions, f)
    }

    l.Infof("Mounting %s at %s with fstype %s", source, targetPath, fsType)
    err = s.mountVolume(source, targetPath, fsType, mountOptions, permissions)
    if err != nil {
        return nil, err
    }

    l.Infof("Device %s staged at %s", source, targetPath)
    return &csi.NodeStageVolumeResponse{}, nil
}

// GetMountPointPermissions - check if mountPoint persmissions were set in config or use default
func (s *NodeServer) GetMountPointPermissions(volumeContext map[string]string) (os.FileMode, error) {
    l := s.log.WithField("func", "GetMountPointPermissions()")
    l.Infof("volumeContext: '%+v'", volumeContext)
    mountPointPermissions := volumeContext["mountPointPermissions"]
    if mountPointPermissions == "" {
        l.Infof("mountPointPermissions is not set, using default: '%+v'", strconv.FormatInt(
            int64(DefaultMountPointPermissions), 8))
        return os.FileMode(DefaultMountPointPermissions), nil
    }
    octalPerm, err := strconv.ParseInt(mountPointPermissions, 8, 16)
    if err != nil {
        return 0, err
    }
    return os.FileMode(octalPerm), nil
}


// NodeUnstageVolume - unstage volume
func (s *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (
    *csi.NodeUnstageVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "NodeUnstageVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    volumeID := req.GetVolumeId()
    if len(volumeID) == 0 {
        return nil, status.Error(codes.InvalidArgument, "req.VolumeId must be provided")
    }

    targetPath := req.GetStagingTargetPath()
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
            return &csi.NodeUnstageVolumeResponse{}, nil
        }
    } else {
        err = nsProvider.DestroyLunMapping(getLunResp.Id)
        if err != nil{
            return nil, err
        }
    }

    // Raw block devices
    if strings.HasSuffix(targetPath, "/device") {
        symLink := filepath.Join(targetPath, "device")
        cmd := exec.Command("realpath", symLink)
        l.Debugf("Executing command: %+v", cmd)
        out, err := cmd.CombinedOutput()
        if err != nil {
            l.Errorf("Command output: %+v", string(out))
            return nil, err
        }
        dev := strings.TrimSpace(string(out))
        err = s.RemoveDevice(dev)
        if err != nil {
            return nil, err
        }
        if err := os.RemoveAll(targetPath); err != nil {
            if os.IsNotExist(err) {
                l.Infof("mount point '%s' already doesn't exist: '%s', return OK", targetPath, err)
                return &csi.NodeUnstageVolumeResponse{}, nil
            } else {
                return nil, err
            }
        }
        return &csi.NodeUnstageVolumeResponse{}, nil
    }

    // Mounted devices
    dev, err := s.DeviceFromTargetPath(targetPath)
    if err != nil {
        if strings.Contains(err.Error(), "Could not determine device path") {
            return &csi.NodeUnstageVolumeResponse{}, nil
        }
        return nil, err
    }

    mounter := mount.New("")
    notMountPoint, err := mounter.IsLikelyNotMountPoint(targetPath)
    if err != nil {
        if os.IsNotExist(err) {
            l.Warnf("mount point '%s' already doesn't exist: '%s', return OK", targetPath, err)
            return &csi.NodeUnstageVolumeResponse{}, nil
        }
        return nil, status.Errorf(
            codes.Internal,
            "Cannot ensure that targetPath '%s' is a mount point: '%s'",
            targetPath,
            err,
        )
    }

    if notMountPoint {
        if err := os.RemoveAll(targetPath); err != nil {
            l.Infof("Remove targetPath error: %s", err.Error())
        }
        return &csi.NodeUnstageVolumeResponse{}, nil
    }

    if err := mounter.Unmount(targetPath); err != nil {
        return nil, status.Errorf(codes.Internal, "Failed to unmount targetPath '%s': %s", targetPath, err)
    }

    err = s.RemoveDevice(dev)
    if err != nil {
        return nil, err
    }

    if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
        return nil, status.Errorf(codes.Internal, "Cannot remove unmounted target path '%s': %s", targetPath, err)
    }
    return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume - mounts NS fs to the node
func (s *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (
    *csi.NodePublishVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "NodePublishVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    volumeID := req.GetVolumeId()

    if len(volumeID) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
    }

    source := req.GetStagingTargetPath()
    if len(source) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Staging targetPath not provided")
    }

    targetPath := req.GetTargetPath()
    if len(targetPath) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Target path not provided")
    }

    volumeCapability := req.GetVolumeCapability()
    if volumeCapability == nil {
        return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
    }
    permissions, err := s.GetMountPointPermissions(req.GetVolumeContext())
    if err != nil {
        return nil, err
    }

    // Make dir if dir not present
    _, err = os.Stat(targetPath)
    if os.IsNotExist(err) {
        if err = os.MkdirAll(filepath.Dir(targetPath), permissions); err != nil {
            return nil, status.Error(codes.Internal, err.Error())
        }
    }

    devName := ""
    switch volumeCapability.GetAccessType().(type) {
    case *csi.VolumeCapability_Block:
        source = filepath.Join(source, "device")
        cmd := exec.Command("realpath", source)
        l.Debugf("Executing command: %+v", cmd)
        out, err := cmd.CombinedOutput()
        if err != nil {
            l.Errorf("Command output: %+v", string(out))
            return nil, err
        }
        devName = strings.TrimSpace(string(out))
        cmd = exec.Command("ln", "-s", devName, targetPath)
        l.Debugf("Executing command: %+v", cmd)
        _, err = cmd.CombinedOutput()
        if err != nil {
            return nil, err
        }
        l.Infof("Device %s published to %s successfully", devName, targetPath)
        return &csi.NodePublishVolumeResponse{}, nil
    case *csi.VolumeCapability_Mount:
        devName, err = s.DeviceFromTargetPath(source)
        if err != nil {
            return nil, err
        }
        fsType := volumeCapability.GetMount().GetFsType()
        mountOptions := []string{}
        if req.GetReadonly() {
            mountOptions = append(mountOptions, "ro")
        }
        err = s.mountVolume(devName, targetPath, fsType, mountOptions, permissions)
        if err != nil {
            return nil, err
        }
    }

    l.Infof("Device %s published to %s successfully", devName, targetPath)
    return &csi.NodePublishVolumeResponse{}, nil
}

func (s *NodeServer) CreateUpdateHostGroup(nsProvider ns.ProviderInterface) (name string, err error) {
    l := s.log.WithField("func", "CreateUpdateHostGroup()")
    nodeIQN, err := s.GetNodeIQN()
    if err != nil {
        return name, err
    }
    hostGroups, err := nsProvider.GetHostGroups()
    if err != nil {
        return name, err
    }
    for _, group := range hostGroups {
        for _, member := range group.Members {
            if member == nodeIQN {
                return group.Name, nil
            }
        }
    }

    hgUUID := uuid.New()
    name = fmt.Sprintf("%s-%s", HostGroupPrefix, hgUUID)
    l.Debugf("name: %v, nodeIQN: %v", name, nodeIQN)
    params := ns.CreateHostGroupParams{
        Name: name,
        Members: []string{nodeIQN},
    }
    err = nsProvider.CreateHostGroup(params)
    if err != nil {
        return name, err
    }
    l.Infof("Successfully created host group: %v with members [%v]", name, nodeIQN)
    return name, nil
}

func (s *NodeServer) GetNodeIQN() (initiatorName string, err error) {
    content, err := ioutil.ReadFile(PathToInitiatorName)
    if err != nil {
        return initiatorName, err
    }

    lines := strings.Split(string(content), "\n")
    for _, line := range lines {
        if strings.HasPrefix(line, "InitiatorName=") && len(line) > 0 {
            initiatorName = strings.Split(line, "InitiatorName=")[1]
        }
    }
    if initiatorName == "" {
        return initiatorName, fmt.Errorf("Node's initiatorname must not be empty")
    }
    return initiatorName, nil
}

func (s *NodeServer) CreateISCSIMapping(params CreateMappingParams, nsProvider ns.ProviderInterface) error {
    l := s.log.WithField("func", "CreateISCSIMapping()")
    l.Infof("Creating iSCSI mapping with params: %+v", params)

    return nsProvider.CreateLunMapping(ns.CreateLunMappingParams{
        Volume: params.VolumePath,
        TargetGroup: params.TargetGroup,
        HostGroup: params.HostGroup,
    })
}

func (s *NodeServer) mountVolume(devName, targetPath, fsType string, mountOptions []string, permissions os.FileMode) error {
    l := s.log.WithField("func", "mountVolume()")
    l.Infof("Mounting device %s to targetPath %s with options %s", devName, targetPath, mountOptions)

    mounter := mount.New("")
    notMnt, err := mounter.IsLikelyNotMountPoint(targetPath)
    if err != nil {
        if os.IsNotExist(err) {
            if err := os.MkdirAll(targetPath, permissions); err != nil {
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

    l.Debugf("target %v, fstype %v, mountOptions %v", targetPath, fsType, mountOptions)
    err = mounter.Mount(devName, targetPath, fsType, mountOptions)
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

    err = os.Chmod(targetPath, permissions)
    if err != nil {
        return err
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
        l.Debugf("Trying to format %s via %s", device, fstype)
        switch fstype {
        case "xfs":
            cmd := exec.Command("mkfs.xfs", "-K", "-f", device)
            l.Debugf("Executing command: %+v", cmd)
            _, err = cmd.CombinedOutput()
        case "ext3":
            cmd := exec.Command("mkfs.ext3", "-E", "nodiscard", "-F", device)
            l.Debugf("Executing command: %+v", cmd)
            _, err = cmd.CombinedOutput()
        case "ext4":
            cmd := exec.Command("mkfs.ext4", "-E", "nodiscard", "-F", device)
            l.Debugf("Executing command: %+v", cmd)
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
        l.Info("Format failed, retrying. Duration: ", duration)
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
        return nil, status.Error(codes.InvalidArgument, "req.VolumeId must be provided")
    }

    targetPath := req.GetTargetPath()
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
            "Cannot ensure that targetPath '%s' is a mount point: '%s'",
            targetPath,
            err,
        )
    }

    if notMountPoint {
        if err := os.RemoveAll(targetPath); err != nil {
            l.Infof("Remove targetPath error: %s", err.Error())
        }
        return &csi.NodeUnpublishVolumeResponse{}, nil
    }

    if err := mounter.Unmount(targetPath); err != nil {
        if !strings.Contains(err.Error(), "not mounted") {
            return nil, status.Errorf(codes.Internal, "Failed to unmount target path '%s': %s", targetPath, err)
        }
    }

    if err := os.RemoveAll(targetPath); err != nil && !os.IsNotExist(err) {
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
        Capabilities: []*csi.NodeServiceCapability{
            &csi.NodeServiceCapability{
                Type: &csi.NodeServiceCapability_Rpc{
                    Rpc: &csi.NodeServiceCapability_RPC{
                        Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
                    },
                },
            },
            &csi.NodeServiceCapability{
                Type: &csi.NodeServiceCapability_Rpc{
                    Rpc: &csi.NodeServiceCapability_RPC{
                        Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
                    },
                },
            },
        },
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

func (s *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (
    *csi.NodeExpandVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "NodeExpandVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    volumeID := req.GetVolumeId()
    if len(volumeID) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
    }

    targetPath := req.GetStagingTargetPath()
    if len(targetPath) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Staging targetPath not provided")
    }

    volumeCapability := req.GetVolumeCapability()
    if volumeCapability == nil {
        return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
    }

    volumePath := req.GetVolumePath()
    if len(volumePath) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Staging volumePath not provided")
    }

    switch volumeCapability.GetAccessType().(type) {
    case *csi.VolumeCapability_Mount:
        devName, err := s.DeviceFromTargetPath(volumePath)
        if err != nil {
            return nil, err
        }
        err = s.RescanDevice(devName)
        if err != nil {
            return nil, err
        }
        r := resizefs.NewResizeFs(&mount.SafeFormatAndMount{
            Interface: mount.New(""),
            Exec:      utilexec.New(),
        })
        if _, err = r.Resize(devName, volumePath); err != nil {
            return nil, status.Errorf(
                codes.Internal, "Could not resize volume %q (%q):  %v", volumeID, devName, err)
        }
    case *csi.VolumeCapability_Block:
        err := s.RescanDevice(volumePath)
        if err != nil {
            return nil, err
        }
    }

    return &csi.NodeExpandVolumeResponse{}, nil
}

func (s *NodeServer) DeviceFromTargetPath(volumePath string) (deviceName string, err error) {
    l := s.log.WithField("func", "DeviceFromTargetPath()")

    cmd := exec.Command("findmnt", "-o", "source", "--noheadings", "--target", volumePath)
    l.Debugf("Executing command: %+v", cmd)
    out, err := cmd.CombinedOutput()
    if err != nil {
        return "", status.Errorf(codes.Internal, "Could not determine device path: %v", err)
    } else {
        l.Debugf("Command output: %+v", string(out))
    }
    devicePath := strings.TrimSpace(string(out))
    return devicePath, nil
}

func (s *NodeServer) getBlockSizeBytes(devicePath string) (int64, error) {
    l := s.log.WithField("func", "getBlockSizeBytes()")
    cmd := exec.Command("blockdev", "--getsize64", devicePath)
    l.Debugf("Executing command: %+v", cmd)
    out, err := cmd.CombinedOutput()
    if err != nil {
        return -1, fmt.Errorf("error when getting size of block volume at path %s: output: %s, err: %v", devicePath, string(out), err)
    } else {
        l.Debugf("Command output: %+v", string(out))
    }
    strOut := strings.TrimSpace(string(out))
    gotSizeBytes, err := strconv.ParseInt(strOut, 10, 64)
    if err != nil {
        return -1, fmt.Errorf("failed to parse size %s as int", strOut)
    }
    return gotSizeBytes, nil
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

// IsBlock checks if the given path is a block device
func (s *NodeServer) IsBlockDevice(fullPath string) (bool, error) {
    var st unix.Stat_t
    err := unix.Stat(fullPath, &st)
    if err != nil {
        return false, err
    }

    return (st.Mode & unix.S_IFMT) == unix.S_IFBLK, nil
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
    l.Debugf("Executing command: %+v", cmd)
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
