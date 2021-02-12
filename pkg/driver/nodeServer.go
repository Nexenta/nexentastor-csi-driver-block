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
    Address     string
    Target      string
    TargetGroup string
    Port        string
    VolumePath  string
    HostGroup   string
}

const (
    defaultFsType = "ext4"
    DefaultISCSIPort = "3260"
    HostGroupPrefix = "csi"
    PathToInitiatorName = "/host/etc/iscsi/initiatorname.iscsi"
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

func (s *NodeServer) ParseVolumeContext(
    volumeContext map[string]string, nsProvider ns.ProviderInterface, configName string) (
    hostGroup, targetGroup, port, dataIP, iSCSITarget string,
    err error,
) {
    cfg := s.config.NsMap[configName]
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
    targetGroup = volumeContext["TargetGroup"]
    if targetGroup == "" {
        targetGroup = cfg.DefaultTargetGroup
    }
    port = volumeContext["iSCSIPort"]
    if port == "" {
        if cfg.DefaultISCSIPort != "" {
            port = cfg.DefaultISCSIPort
        } else {
            port = DefaultISCSIPort
        }
    }
    dataIP = volumeContext["dataIP"]
    if dataIP == "" {
        dataIP = cfg.DefaultDataIP
    }
    iSCSITarget = volumeContext["Target"]
    if iSCSITarget == "" {
        iSCSITarget = cfg.DefaultTarget
    }
    return hostGroup, targetGroup, port, dataIP, iSCSITarget, nil
}

func (s *NodeServer) ConstructDevByPath(portal, iSCSITarget string, lunNumber int) (devByPath string) {
    return strings.Join([]string{
        "/dev/disk/by-path/ip", portal,
        "iscsi", iSCSITarget, "lun", strconv.Itoa(lunNumber)}, "-")
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
    params := ns.GetLunMappingsParams{
        TargetGroup: targetGroup,
        Volume: volumePath,
        HostGroup: hostGroup,
    }
    lunMappings, err := nsProvider.GetLunMappings(params)
    if err != nil {
        return nil, err
    }
    if len(lunMappings) == 0 {
        params := CreateMappingParams{
            Address: dataIP,
            Target: iSCSITarget,
            TargetGroup: targetGroup,
            Port: port,
            VolumePath: volumePath,
            HostGroup: hostGroup,
        }
        err = s.CreateISCSIMapping(params, nsProvider)
        if err != nil {
            return nil, err
        }
    }

    device := ""
    _, err = os.Stat(targetPath)
    if os.IsNotExist(err) {
        if err = os.MkdirAll(filepath.Dir(targetPath), 0750); err != nil {
            return nil, status.Error(codes.Internal, err.Error())
        }
    } else {
        device, err = s.MountFromTargetPath(targetPath)
        if err != nil {
            device = ""
            // return nil, err
        }
    }

    portal := fmt.Sprintf("%s:%s", dataIP, port)
    err = s.ISCSILogInRescan(iSCSITarget, portal)
    if err != nil {
        return nil, err
    }

    getLunResp, err := nsProvider.GetLunMapping(volumePath)
    if err != nil{
        return nil, err
    }
    lunNumber := getLunResp.Lun
    devByPath := s.ConstructDevByPath(portal, iSCSITarget, lunNumber)
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
        cmd := exec.Command("ln", "-s", source, targetPath)
        l.Infof("Executing command: %+v", cmd)
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
        fsType = defaultFsType
    }
    deviceFS := s.getFSType(source)
    if deviceFS == "" {
        if fsType == "" {
            fsType = defaultFsType
        }
        err = s.formatVolume(source, fsType)
        if err != nil {
            return nil, err
        }
    } else if deviceFS != fsType {
        return nil, fmt.Errorf(
            "Volume %s is already formatted in %s, requested: %s,", volumeID, deviceFS, fsType)
    } else {
        cmd := exec.Command("e2fsck", "-f", "-y", source)
        l.Infof("Executing command: %+v", cmd)
        _, err = cmd.CombinedOutput()
        if err != nil {
            return nil, err
        }
        r := resizefs.NewResizeFs(&mount.SafeFormatAndMount{
            Interface: mount.New(""),
            Exec:      utilexec.New(),
        })

        if _, err := r.Resize(source, targetPath); err != nil {
            return nil, status.Errorf(
                codes.Internal, "Could not resize volume %q (%q):  %v", volumeID, source, err)
        }
    }

    var mountOptions []string
    for _, f := range capabilityMount.MountFlags {
        mountOptions = append(mountOptions, f)
    }

    l.Infof("Mounting %s at %s with fstype %s", source, targetPath, fsType)
    err = s.mountVolume(source, targetPath, fsType, mountOptions)
    if err != nil {
        return nil, err
    }

    return &csi.NodeStageVolumeResponse{}, nil
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

    port := cfg.DefaultISCSIPort
    if port == "" {
        port = DefaultISCSIPort
    }
    dataIP := cfg.DefaultDataIP
    portal := fmt.Sprintf("%s:%s", dataIP, port)
    iSCSITarget := cfg.DefaultTarget
    lunNumber := getLunResp.Lun
    devByPath := s.ConstructDevByPath(portal, iSCSITarget, lunNumber)

    dev, err := s.GetRealDeviceName(devByPath)
    if err != nil {
        return nil, err
    }

    err = s.RemoveDevice(dev)
    if err != nil {
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
        if err := os.Remove(targetPath); err != nil {
            l.Infof("Remove targetPath error: %s", err.Error())
        }
        return &csi.NodeUnstageVolumeResponse{}, nil
    }

    if err := mounter.Unmount(targetPath); err != nil {
        return nil, status.Errorf(codes.Internal, "Failed to unmount targetPath '%s': %s", targetPath, err)
    }

    if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
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
    volumeContext := req.GetVolumeContext()

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

    _, _, port, dataIP, iSCSITarget, err := s.ParseVolumeContext(
        volumeContext, nsProvider, configName)
    if err != nil {
        return nil, err
    }

    portal := fmt.Sprintf("%s:%s", dataIP, port)
    devByPath := s.ConstructDevByPath(portal, iSCSITarget, lunNumber)

    devName, err := s.GetRealDeviceName(devByPath)
    if err != nil {
        return nil, err
    }

    switch volumeCapability.GetAccessType().(type) {
    case *csi.VolumeCapability_Block:
        cmd := exec.Command("ln", "-s", devName, targetPath)
        l.Infof("Executing command: %+v", cmd)
        _, err = cmd.CombinedOutput()
        if err != nil {
            return nil, err
        }
        return &csi.NodePublishVolumeResponse{}, nil
    case *csi.VolumeCapability_Mount:
        fsType := volumeCapability.GetMount().GetFsType()
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

        mountOptions := []string{"bind"}
        if req.GetReadonly() {
            mountOptions = append(mountOptions, "ro")
        }
        err = s.mountVolume(source, targetPath, fsType, mountOptions)
        if err != nil {
            return nil, err
        }
    }

    l.Infof("Device %s published to %s", devName, targetPath)
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
    l.Infof("name: %v, nodeIQN: %v", name, nodeIQN)
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

func (s *NodeServer) mountVolume(devName, targetPath, fsType string, mountOptions []string) error {
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

    l.Infof("target %v, fstype %v, mountOptions %v", targetPath, fsType, mountOptions)
    err = mounter.Mount(fmt.Sprintf("/host%s", devName), targetPath, fsType, mountOptions)
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
        if err := os.Remove(targetPath); err != nil {
            l.Infof("Remove targetPath error: %s", err.Error())
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
    return &csi.NodeExpandVolumeResponse{}, nil
}

func (s *NodeServer) MountFromTargetPath(volumePath string) (deviceName string, err error) {
    l := s.log.WithField("func", "MountFromTargetPath()")

    cmd := exec.Command("findmnt", "-o", "source", "--noheadings", "--target", volumePath)
    l.Infof("Executing command: %+v", cmd)
    out, err := cmd.CombinedOutput()
    if err != nil {
        return "", status.Errorf(codes.Internal, "Could not determine device path: %v", err)
    } else {
        l.Infof("Command output: %+v", string(out))
    }
    splittedOut := strings.Split(strings.TrimSpace(string(out)), "/host")
    if len(splittedOut) != 2 {
        return "", status.Error(codes.InvalidArgument, fmt.Sprintf("Device path is in wrong format: %s", string(out)))
    }
    devicePath := splittedOut[1]
    return devicePath, nil
}

func (s *NodeServer) getBlockSizeBytes(devicePath string) (int64, error) {
    l := s.log.WithField("func", "getBlockSizeBytes()")
    cmd := exec.Command("blockdev", "--getsize64", devicePath)
    l.Infof("Executing command: %+v", cmd)
    out, err := cmd.CombinedOutput()
    if err != nil {
        return -1, fmt.Errorf("error when getting size of block volume at path %s: output: %s, err: %v", devicePath, string(out), err)
    } else {
        l.Infof("Command output: %+v", string(out))
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
