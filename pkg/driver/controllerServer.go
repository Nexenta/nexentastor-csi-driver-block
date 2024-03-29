package driver

import (
    "fmt"
    "path/filepath"
    "strconv"
    "strings"
    "time"

    "github.com/container-storage-interface/spec/lib/go/csi"
    // "google.golang.org/protobuf/ptypes"
    "google.golang.org/protobuf/types/known/timestamppb"
    "github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
    "github.com/sirupsen/logrus"
    "golang.org/x/net/context"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"

    "github.com/Nexenta/go-nexentastor/pkg/ns"
    "github.com/Nexenta/nexentastor-csi-driver-block/pkg/config"
)

const TopologyKeyZone = "topology.kubernetes.io/zone"
const DefaultSparseVolume = true

// supportedControllerCapabilities - driver controller capabilities
var supportedControllerCapabilities = []csi.ControllerServiceCapability_RPC_Type{
    csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
    csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
    csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
    csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
    csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
    csi.ControllerServiceCapability_RPC_GET_CAPACITY,
    csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
    csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
}

// supportedVolumeCapabilities - driver volume capabilities
var supportedVolumeCapabilities = []*csi.VolumeCapability{
    {
        AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY},
    },
    {
        AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
    },
    {
        AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY},
    },
    {
        AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER},
    },
    {
        AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
    },
}

// ControllerServer - k8s csi driver controller server
type ControllerServer struct {
    nsResolverMap   map[string]ns.Resolver
    config          *config.Config
    log             *logrus.Entry
}

type ResolveNSParams struct {
    volumeGroup string
    zone        string
    configName  string
}

type ResolveNSResponse struct {
    volumeGroup string
    nsProvider  ns.ProviderInterface
    configName  string
}

func (s *ControllerServer) refreshConfig(secret string) error {
    changed, err := s.config.Refresh(secret)
    if err != nil {
        return err
    }
    if changed {
        s.log.Info("config has been changed, updating...")
        for name, cfg := range s.config.NsMap {
            resolver, err := ns.NewResolver(ns.ResolverArgs{
                Address:            cfg.Address,
                Username:           cfg.Username,
                Password:           cfg.Password,
                Log:                s.log,
                InsecureSkipVerify: *cfg.InsecureSkipVerify,
            })
            s.nsResolverMap[name] = *resolver
            if err != nil {
                return fmt.Errorf("Cannot create NexentaStor resolver: %s", err)
            }
        }
    }

    return nil
}

func (s *ControllerServer) resolveNS(params ResolveNSParams) (response ResolveNSResponse, err error) {
    l := s.log.WithField("func", "resolveNS()")
    l.Infof("Resolving NS with params: %+v", params)
    if len(params.zone) == 0 {
        response, err = s.resolveNSNoZone(params)
    } else {
        response, err = s.resolveNSWithZone(params)
    }
    if err != nil {
        code := codes.Internal
        if ns.IsNotExistNefError(err) {
            code = codes.NotFound
        }
        return response, status.Errorf(
            code,
            "Cannot resolve '%s' on any NexentaStor(s): %s",
            params.volumeGroup,
            err,
        )
    } else {
        l.Infof("resolved NS: [%s], %s, %s", response.configName, response.nsProvider, response.volumeGroup)
        return response, nil
    }
}

func (s *ControllerServer) resolveNSNoZone(params ResolveNSParams) (response ResolveNSResponse, err error) {
    // No zone -> pick NS for given volumeGroup and configName. TODO: load balancing
    l := s.log.WithField("func", "resolveNSNoZone()")
    l.Infof("Resolving without zone, params: %+v", params)
    var nsProvider ns.ProviderInterface
    volumeGroup := params.volumeGroup
    if len(params.configName) > 0 {
        if volumeGroup == "" {
            volumeGroup = s.config.NsMap[params.configName].DefaultVolumeGroup
        }
        resolver := s.nsResolverMap[params.configName]
        nsProvider, err = resolver.ResolveFromVg(volumeGroup)
        if err != nil {
            return response, err
        }
        response = ResolveNSResponse{
            volumeGroup: volumeGroup,
            nsProvider: nsProvider,
            configName: params.configName,
        }
        return response, nil
    } else {
        for name, resolver := range s.nsResolverMap {
            if params.volumeGroup == "" {
                volumeGroup = s.config.NsMap[name].DefaultVolumeGroup
            }
            nsProvider, err = resolver.ResolveFromVg(volumeGroup)
            if nsProvider != nil {
                response = ResolveNSResponse{
                    volumeGroup: volumeGroup,
                    nsProvider: nsProvider,
                    configName: name,
                }
                return response, err
            }
        }
    }

    if strings.Contains(err.Error(), "unknown authority") {
        return response, status.Errorf(
            codes.Unauthenticated, fmt.Sprintf("TLS certificate check error: %v", err.Error()))
    }
    return response, status.Errorf(codes.NotFound, fmt.Sprintf("No nsProvider found for params: %+v", params))
}

func (s *ControllerServer) resolveNSWithZone(params ResolveNSParams) (response ResolveNSResponse, err error) {
    // Pick NS with corresponding zone. TODO: load balancing
    l := s.log.WithField("func", "resolveNSWithZone()")
    l.Infof("Resolving with zone, params: %+v", params)
    var nsProvider ns.ProviderInterface
    volumeGroup := params.volumeGroup
    if len(params.configName) > 0 {
        if s.config.NsMap[params.configName].Zone != params.zone {
            msg := fmt.Sprintf(
                "requested zone [%s] does not match requested NexentaStor name [%s]", params.zone, params.configName)
            return response, status.Errorf(codes.FailedPrecondition, msg)
        }
        if volumeGroup == "" {
            volumeGroup = s.config.NsMap[params.configName].DefaultVolumeGroup
        }
        resolver := s.nsResolverMap[params.configName]
        nsProvider, err = resolver.ResolveFromVg(volumeGroup)
        if err != nil {
            return response, err
        }
        response = ResolveNSResponse{
            volumeGroup: volumeGroup,
            nsProvider: nsProvider,
            configName: params.configName,
        }
        return response, nil
    } else {
        for name, resolver := range s.nsResolverMap {
            if params.zone == s.config.NsMap[name].Zone {
                if volumeGroup == "" {
                    volumeGroup = s.config.NsMap[name].DefaultVolumeGroup
                }
                nsProvider, err = resolver.ResolveFromVg(volumeGroup)
                if nsProvider != nil {
                    l.Infof("Found volumeGroup %s on NexentaStor [%s]", volumeGroup, name)
                    response = ResolveNSResponse{
                        volumeGroup: volumeGroup,
                        nsProvider: nsProvider,
                        configName: name,
                    }
                    l.Infof("configName: %+v", name)
                    return response, nil
                }
            }
        }
    }

    if strings.Contains(err.Error(), "unknown authority") {
        return response, status.Errorf(
            codes.Unauthenticated, fmt.Sprintf("TLS certificate check error: %v", err.Error()))
    }
    return response, status.Errorf(codes.NotFound, fmt.Sprintf("No nsProvider found for params: %+v", params))
}

// ValidateVolumeCapabilities validates volume capabilities
// Shall return confirmed only if all the volume
// capabilities specified in the request are supported.
func (s *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (
    *csi.ValidateVolumeCapabilitiesResponse,
    error,
) {
    l := s.log.WithField("func", "ValidateVolumeCapabilities()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    volumeId := req.GetVolumeId()
    if len(volumeId) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Volume ID must be provided")
    }
    splittedVol := strings.Split(volumeId, ":")
    if len(splittedVol) != 2 {
        return nil, status.Error(codes.NotFound, fmt.Sprintf("VolumeId is in wrong format: %s", volumeId))
    }

    volumeCapabilities := req.GetVolumeCapabilities()
    if volumeCapabilities == nil {
        return nil, status.Error(codes.InvalidArgument, "req.VolumeCapabilities must be provided")
    }

    // volume attributes are passed from ControllerServer.CreateVolume()
    volumeContext := req.GetVolumeContext()

    var secret string
    secrets := req.GetSecrets()
    for _, v := range secrets {
        secret = v
    }
    err := s.refreshConfig(secret)
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }

    for _, reqC := range volumeCapabilities {
        supported := validateVolumeCapability(reqC)
        l.Infof("requested capability: '%s', supported: %t", reqC.GetAccessMode().GetMode(), supported)
        if !supported {
            message := fmt.Sprintf("Driver does not support volume capability mode: %s", reqC.GetAccessMode().GetMode())
            l.Warn(message)
            return &csi.ValidateVolumeCapabilitiesResponse{
                Message: message,
            }, nil
        }
    }

    return &csi.ValidateVolumeCapabilitiesResponse{
        Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
            VolumeCapabilities: supportedVolumeCapabilities,
            VolumeContext:      volumeContext,
        },
    }, nil
}

func validateVolumeCapability(requestedVolumeCapability *csi.VolumeCapability) bool {
    requestedMode := requestedVolumeCapability.GetAccessMode().GetMode()

    for _, volumeCapability := range supportedVolumeCapabilities {
        if volumeCapability.GetAccessMode().GetMode() == requestedMode {
            return true
        }
    }
    return false
}

func (s *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (
    *csi.ControllerExpandVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "ControllerExpandVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    var secret string
    secrets := req.GetSecrets()
    for _, v := range secrets {
        secret = v
    }
    err := s.refreshConfig(secret)
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }
    capacityBytes := req.GetCapacityRange().GetRequiredBytes()
    if capacityBytes == 0 {
        return nil, status.Error(codes.InvalidArgument, "GetRequiredBytes must be >0")
    }

    volumeId := req.GetVolumeId()
    if len(volumeId) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Volume ID must be provided")
    }
    splittedVol := strings.Split(volumeId, ":")
    if len(splittedVol) != 2 {
        return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeId is in wrong format: %s", volumeId))
    }
    configName, volumePath := splittedVol[0], splittedVol[1]

    splittedPath := strings.Split(volumePath, "/")
    if len(splittedPath) != 3 {
        l.Infof("Got wrong volumeId, but that is OK for deletion")
        return &csi.ControllerExpandVolumeResponse{}, nil
    }
    volumeGroup := strings.Join(splittedPath[:2], "/")

    params := ResolveNSParams{
        volumeGroup: volumeGroup,
        configName: configName,
    }
    resolveResp, err := s.resolveNS(params)
    if err != nil {
        return nil, err
    }
    nsProvider := resolveResp.nsProvider

    // Check if volume was not already expanded
    l.Debugf("Checking volume %s size", volumePath)
    volInfo, err := nsProvider.GetVolume(volumePath)
    if err != nil {
        return nil, err
    }
    currentSize := volInfo.VolumeSize
    l.Debugf("Current size of volume %s = %+v", volumePath, currentSize)

    if currentSize < capacityBytes {
        l.Infof("expanding volume %+v to %+v bytes", volumePath, capacityBytes)
        err = nsProvider.UpdateVolume(volumePath, ns.UpdateVolumeParams{
            VolumeSize: capacityBytes,
        })
        if err != nil {
            return nil, fmt.Errorf("Failed to expand volume %s: %s", volumePath, err)
        }
        l.Debugf("expanded volume %+v successfully.", volumePath)
        return &csi.ControllerExpandVolumeResponse{
            CapacityBytes: capacityBytes,
            NodeExpansionRequired: true,
        }, nil
    }
    return &csi.ControllerExpandVolumeResponse{
        CapacityBytes: capacityBytes,
    }, nil
}

func (s *ControllerServer) pickAvailabilityZone(requirement *csi.TopologyRequirement) string {
    l := s.log.WithField("func", "s.pickAvailabilityZone()")
    l.Infof("AccessibilityRequirements: '%+v'", requirement)
    if requirement == nil {
        return ""
    }
    for _, topology := range requirement.GetPreferred() {
        zone, exists := topology.GetSegments()[TopologyKeyZone]
        if exists {
            return zone
        }
    }
    for _, topology := range requirement.GetRequisite() {
        zone, exists := topology.GetSegments()[TopologyKeyZone]
        if exists {
            return zone
        }
    }
    return ""
}

func (s *ControllerServer) ControllerGetVolume(
    ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error,
) {
    return nil, status.Errorf(codes.Unimplemented, "method CreateVolume not implemented")
}

func (s *ControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (
    *csi.GetCapacityResponse,
    error,
) {
    l := s.log.WithField("func", "GetCapacity()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    reqParams := req.GetParameters()
    if reqParams == nil {
        reqParams = make(map[string]string)
    }

    // get volumeGroup path from runtime params, set default if not specified
    volumeGroup := ""
    if v, ok := reqParams["volumeGroup"]; ok {
        volumeGroup = v
    }

    params := ResolveNSParams{
        volumeGroup: volumeGroup,
    }
    resolveResp, err := s.resolveNS(params)
    if err != nil {
        return nil, err
    }

    nsProvider := resolveResp.nsProvider
    vgData, err := nsProvider.GetVolumeGroup(resolveResp.volumeGroup)
    if err != nil {
        return nil, err
    }
    availableCapacity := vgData.BytesAvailable
    l.Infof("Available capacity: '%+v' bytes", availableCapacity)
    return &csi.GetCapacityResponse{
        AvailableCapacity: availableCapacity,
    }, nil
}

// CreateVolume - creates volume on NexentaStor
func (s *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (
    res *csi.CreateVolumeResponse,
    err error,
) {
    l := s.log.WithField("func", "CreateVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))
    volumeName := req.GetName()
    if len(volumeName) == 0 {
        return nil, status.Error(codes.InvalidArgument, "req.Name must be provided")
    }
    var secret string
    secrets := req.GetSecrets()
    for _, v := range secrets {
        secret = v
    }

    err = s.refreshConfig(secret)
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }

    volumeCapabilities := req.GetVolumeCapabilities()
    if volumeCapabilities == nil {
        return nil, status.Error(codes.InvalidArgument, "req.VolumeCapabilities must be provided")
    }
    for _, reqC := range volumeCapabilities {
        supported := validateVolumeCapability(reqC)
        if !supported {
            message := fmt.Sprintf("Driver does not support volume capability mode: %s", reqC.GetAccessMode().GetMode())
            l.Warn(message)
            return nil, status.Error(codes.FailedPrecondition, message)
        }
    }

    reqParams := req.GetParameters()
    if reqParams == nil {
        reqParams = make(map[string]string)
    }
    volumeGroup := ""
    if v, ok := reqParams["volumeGroup"]; ok {
        volumeGroup = v
    }
    configName := ""
    if v, ok := reqParams["configName"]; ok {
        configName = v
    }

    sparseVolume := DefaultSparseVolume
    if v, ok := reqParams["sparseVolume"]; ok {
        sparseVolume, err = strconv.ParseBool(v)
        if err != nil {
            return nil, status.Errorf(
                codes.InvalidArgument,
                "Could not parse sparseVolume parameter = %s, error: %+v",
                v, err.Error(),
            )
        }
    }

    var sourceSnapshotId string
    var sourceVolumeId string
    var volumePath string
    var contentSource *csi.VolumeContentSource
    var nsProvider ns.ProviderInterface
    var resolveResp ResolveNSResponse

    if volumeContentSource := req.GetVolumeContentSource(); volumeContentSource != nil {
        if sourceSnapshot := volumeContentSource.GetSnapshot(); sourceSnapshot != nil {
            sourceSnapshotId = sourceSnapshot.GetSnapshotId()
            contentSource = req.GetVolumeContentSource()
        } else if sourceVolume := volumeContentSource.GetVolume(); sourceVolume != nil {
            sourceVolumeId = sourceVolume.GetVolumeId()
            contentSource = req.GetVolumeContentSource()
        } else {
            return nil, status.Errorf(
                codes.InvalidArgument,
                "Only snapshots and volumes are supported as volume content source, but got type: %s",
                volumeContentSource.GetType(),
            )
        }
    }

    requirements := req.GetAccessibilityRequirements()
    zone := s.pickAvailabilityZone(requirements)
    params := ResolveNSParams{
        volumeGroup: volumeGroup,
        zone: zone,
        configName: configName,
    }

    // get requested volume size from runtime params, set default if not specified
    capacityBytes := req.GetCapacityRange().GetRequiredBytes()
    if capacityBytes == 0 {
        capacityBytes = 1073741824
    }
    if sourceSnapshotId != "" {
        // create new volume using existing snapshot
        splittedSnap := strings.Split(sourceSnapshotId, ":")
        if len(splittedSnap) != 2 {
            return nil, status.Error(codes.NotFound, fmt.Sprintf("SnapshotId is in wrong format: %s", sourceSnapshotId))
        }
        configName, sourceSnapshot := splittedSnap[0], splittedSnap[1]
        params.configName = configName
        resolveResp, err = s.resolveNS(params)
        if err != nil {
            return nil, err
        }
        nsProvider = resolveResp.nsProvider
        volumeGroup = resolveResp.volumeGroup
        volumePath = filepath.Join(volumeGroup, volumeName)
        err = s.createNewVolumeFromSnapshot(nsProvider, sourceSnapshot, volumePath, capacityBytes)
    } else if sourceVolumeId != "" {
        // clone existing volume
        splittedVol := strings.Split(sourceVolumeId, ":")
        if len(splittedVol) != 2 {
            return nil, status.Error(codes.NotFound, fmt.Sprintf("VolumeId is in wrong format: %s", sourceVolumeId))
        }
        configName, sourceVolume := splittedVol[0], splittedVol[1]
        params.configName = configName
        resolveResp, err = s.resolveNS(params)
        if err != nil {
            return nil, err
        }
        nsProvider = resolveResp.nsProvider
        volumeGroup = resolveResp.volumeGroup
        volumePath = filepath.Join(volumeGroup, volumeName)
        err = s.createClonedVolume(nsProvider, sourceVolume, volumePath, volumeName, capacityBytes)
    } else {
        resolveResp, err = s.resolveNS(params)
        if err != nil {
            return nil, err
        }
        nsProvider = resolveResp.nsProvider
        volumeGroup = resolveResp.volumeGroup
        volumePath = filepath.Join(volumeGroup, volumeName)
        err = s.createNewVolume(nsProvider, volumePath, capacityBytes, sparseVolume)
    }

    if err != nil {
        return nil, err
    }

    cfg := s.config.NsMap[resolveResp.configName]
    // get values from req params or set defaults
    dataIP := ""
    if v, ok := reqParams["dataIP"]; ok {
        dataIP = v
    } else {
        dataIP = cfg.DefaultDataIP
    }
    target := ""
    if v, ok := reqParams["target"]; ok {
        target = v
    } else {
        target = cfg.DefaultTarget
    }
    hostGroup := ""
    if v, ok := reqParams["hostGroup"]; ok {
        hostGroup = v
    } else {
        hostGroup = cfg.DefaultHostGroup
    }
    iSCSIPort := ""
    if v, ok := reqParams["iSCSIPort"]; ok {
        iSCSIPort = v
    } else {
        iSCSIPort = cfg.DefaultISCSIPort
    }
    targetGroup := ""
    if v, ok := reqParams["targetGroup"]; ok {
        targetGroup = v
    } else {
        targetGroup = cfg.DefaultTargetGroup
    }
    iSCSITargetPrefix := ""
    if v, ok := reqParams["iSCSITargetPrefix"]; ok {
        iSCSITargetPrefix = v
    } else {
        iSCSITargetPrefix = cfg.ISCSITargetPrefix
    }
    numOfLunsPerTarget := ""
    if v, ok := reqParams["numOfLunsPerTarget"]; ok {
        numOfLunsPerTarget = v
    } else {
        numOfLunsPerTarget = cfg.NumOfLunsPerTarget
    }

    useChapAuth := ""
    if v, ok := reqParams["useChapAuth"]; ok {
        useChapAuth = v
    } else {
        useChapAuth = cfg.UseChapAuth
    }

    chapUser := ""
    if v, ok := reqParams["chapUser"]; ok {
        chapUser = v
    } else {
        chapUser = cfg.ChapUser
    }

    chapSecret := ""
    if v, ok := reqParams["chapSecret"]; ok {
        chapSecret = v
    } else {
        chapSecret = cfg.ChapSecret
    }

    mountPointPermissions := ""
    if v, ok := reqParams["mountPointPermissions"]; ok {
        mountPointPermissions = v
    } else {
        mountPointPermissions = cfg.MountPointPermissions
    }

    res = &csi.CreateVolumeResponse{
        Volume: &csi.Volume{
            ContentSource: contentSource,
            VolumeId:      fmt.Sprintf("%s:%s", resolveResp.configName, volumePath),
            CapacityBytes: capacityBytes,
            VolumeContext: map[string]string{
                "DataIP": dataIP,
                "VolumeGroup": volumeGroup,
                "Target": target,
                "TargetGroup": targetGroup,
                "HostGroup": hostGroup,
                "iSCSIPort": iSCSIPort,
                "iSCSITargetPrefix": iSCSITargetPrefix,
                "numOfLunsPerTarget": numOfLunsPerTarget,
                "useChapAuth": useChapAuth,
                "chapUser": chapUser,
                "chapSecret": chapSecret,
                "mountPointPermissions": mountPointPermissions,
            },
        },
    }
    if len(zone) > 0 {
        res.Volume.AccessibleTopology = []*csi.Topology{
            {
                Segments: map[string]string{TopologyKeyZone: zone},
            },
        }
    }
    return res, nil
}

func (s *ControllerServer) createNewVolume(
    nsProvider ns.ProviderInterface,
    volumePath string,
    capacityBytes int64,
    sparseVolume bool,
) (error) {
    l := s.log.WithField("func", "createNewVolume()")
    l.Infof("nsProvider: %s, volumePath: %s", nsProvider, volumePath)

    err := nsProvider.CreateVolume(ns.CreateVolumeParams{
        Path:                volumePath,
        VolumeSize:          capacityBytes,
        SparseVolume:        sparseVolume,
    })

    if err != nil {
        if ns.IsAlreadyExistNefError(err) {
            existingVolume, err := nsProvider.GetVolume(volumePath)
            if err != nil {
                return status.Errorf(
                    codes.Internal,
                    "Volume '%s' already exists, but volume properties request failed: %s",
                    volumePath,
                    err,
                )
            } else if capacityBytes != 0 && existingVolume.VolumeSize != capacityBytes {
                return status.Errorf(
                    codes.AlreadyExists,
                    "Volume '%s' already exists, but with a different size: requested=%d, existing=%d",
                    volumePath,
                    capacityBytes,
                    existingVolume.VolumeSize,
                )
            }
            l.Infof("volume '%s' already exists and can be used", volumePath)
            return nil
        }

        return status.Errorf(
            codes.Internal,
            "Cannot create volume '%s': %s",
            volumePath,
            err,
        )
    }

    l.Infof("volume '%s' has been created", volumePath)
    return nil
}

// create new volume using existing snapshot
func (s *ControllerServer) createNewVolumeFromSnapshot(
    nsProvider ns.ProviderInterface,
    sourceSnapshotID string,
    volumePath string,
    capacityBytes int64,
) (error) {
    l := s.log.WithField("func", "createNewVolumeFromSnapshot()")
    l.Infof("snapshot: %s", sourceSnapshotID)

    snapshot, err := nsProvider.GetSnapshot(sourceSnapshotID)
    if err != nil {
        message := fmt.Sprintf("Failed to find snapshot '%s': %s", sourceSnapshotID, err)
        if ns.IsNotExistNefError(err) || ns.IsBadArgNefError(err) {
            return status.Error(codes.NotFound, message)
        }
        return status.Error(codes.NotFound, message)
    }

    err = nsProvider.CloneSnapshot(snapshot.Path, ns.CloneSnapshotParams{
        TargetPath: volumePath,
    })
    if err != nil {
        if ns.IsAlreadyExistNefError(err) {
            l.Infof("volume '%s' already exists and can be used", volumePath)
            return nil
        }

        return status.Errorf(
            codes.Internal,
            "Cannot create volume '%s' using snapshot '%s': %s",
            volumePath,
            snapshot.Path,
            err,
        )
    }

    l.Infof("volume '%s' has been created using snapshot '%s'", volumePath, snapshot.Path)
    return nil
}

func (s *ControllerServer) createClonedVolume(
    nsProvider ns.ProviderInterface,
    sourceVolumeID string,
    volumePath string,
    volumeName string,
    capacityBytes int64,
) (error) {

    l := s.log.WithField("func", "createClonedVolume()")
    l.Infof("clone volume source: %+v, target: %+v", sourceVolumeID, volumePath)

    snapName := fmt.Sprintf("k8s-clone-snapshot-%s", volumeName)
    snapshotPath := fmt.Sprintf("%s@%s", sourceVolumeID, snapName)

    _, err := s.CreateSnapshotOnNS(nsProvider, sourceVolumeID, snapName)
    if err != nil {
        return err
    }

    err = nsProvider.CloneSnapshot(snapshotPath, ns.CloneSnapshotParams{
        TargetPath: volumePath,
    })

    if err != nil {
        if ns.IsAlreadyExistNefError(err) {
            l.Infof("volume '%s' already exists and can be used", volumePath)
            return nil
        }

        return status.Errorf(
            codes.NotFound,
            "Cannot create volume '%s' using snapshot '%s': %s",
            volumePath,
            snapshotPath,
            err,
        )
    }

    l.Infof("successfully created cloned volume %+v", volumePath)
    return nil
}

// DeleteVolume - destroys volume on NexentaStor
func (s *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (
    *csi.DeleteVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "DeleteVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    var secret string
    secrets := req.GetSecrets()
    for _, v := range secrets {
        secret = v
    }
    err := s.refreshConfig(secret)
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }

    volumeId := req.GetVolumeId()
    if len(volumeId) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Volume ID must be provided")
    }
    splittedVol := strings.Split(volumeId, ":")
    if len(splittedVol) != 2 {
        l.Infof("Got wrong volumeId, but that is OK for deletion")
        return &csi.DeleteVolumeResponse{}, nil
    }
    configName, volumePath := splittedVol[0], splittedVol[1]

    splittedPath := strings.Split(volumePath, "/")
    if len(splittedPath) != 3 {
        l.Infof("Got wrong volumeId, but that is OK for deletion")
        return &csi.DeleteVolumeResponse{}, nil
    }
    volumeGroup := strings.Join(splittedPath[:2], "/")

    params := ResolveNSParams{
        volumeGroup: volumeGroup,
        configName: configName,
    }
    resolveResp, err := s.resolveNS(params)
    if err != nil {
        if status.Code(err) == codes.NotFound {
            l.Infof("volume '%s' not found, that's OK for deletion request", volumePath)
            return &csi.DeleteVolumeResponse{}, nil
        }
        return nil, err
    }
    nsProvider := resolveResp.nsProvider

    lunMappingParams := ns.GetLunMappingsParams{
        Volume: volumePath,
    }
    luns, err := nsProvider.GetLunMappings(lunMappingParams)
    if err != nil {
        return nil, err
    }
    for _, lun := range luns {
        err = nsProvider.DestroyLunMapping(lun.Id)
        if err != nil{
            return nil, err
        }
    }

    // if here, than volumePath exists on some NS
    err = nsProvider.DestroyVolume(volumePath, ns.DestroyVolumeParams{
        DestroySnapshots:               true,
        PromoteMostRecentCloneIfExists: true,
    })
    if err != nil && !ns.IsNotExistNefError(err) {
        return nil, status.Errorf(
            codes.Internal,
            "Cannot delete '%s' volume: %s",
            volumePath,
            err,
        )
    }

    l.Infof("volume '%s' has been deleted", volumePath)
    return &csi.DeleteVolumeResponse{}, nil
}

// CreateSnapshot creates a snapshot of given volume
func (s *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (
    *csi.CreateSnapshotResponse,
    error,
) {
    l := s.log.WithField("func", "CreateSnapshot()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    err := s.refreshConfig("")
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }

    sourceVolumeId := req.GetSourceVolumeId()
    if len(sourceVolumeId) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Snapshot source volume ID must be provided")
    }
    splittedVol := strings.Split(sourceVolumeId, ":")
    if len(splittedVol) != 2 {
        return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeId is in wrong format: %s", sourceVolumeId))
    }
    configName, volumePath := splittedVol[0], splittedVol[1]

    name := req.GetName()
    if len(name) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Snapshot name must be provided")
    }

    splittedPath := strings.Split(volumePath, "/")
    if len(splittedPath) != 3 {
        return nil, status.Errorf(codes.InvalidArgument, "Got wrong volumeId: %s", sourceVolumeId)
    }
    volumeGroup := strings.Join(splittedPath[:2], "/")

    params := ResolveNSParams{
        volumeGroup: volumeGroup,
        configName:  configName,
    }
    resolveResp, err := s.resolveNS(params)
    if err != nil {
        return nil, err
    }

    snapshotPath := fmt.Sprintf("%s@%s", volumePath, name)
    createdSnapshot, err := s.CreateSnapshotOnNS(resolveResp.nsProvider, volumePath, name)
    if err != nil {
        return nil, err
    }

    creationTime := timestamppb.New(createdSnapshot.CreationTime)
    if err != nil {
        return nil, err
    }

    snapshotId := fmt.Sprintf("%s:%s", configName, snapshotPath)
    res := &csi.CreateSnapshotResponse{
        Snapshot: &csi.Snapshot{
            SnapshotId:     snapshotId,
            SourceVolumeId: sourceVolumeId,
            CreationTime:   creationTime,
            ReadyToUse:     true,
        },
    }

    if ns.IsAlreadyExistNefError(err) {
        l.Infof("snapshot '%s' already exists and can be used", snapshotId)
        return res, nil
    }

    l.Infof("snapshot '%s' has been created", snapshotId)
    return res, nil
}

func (s *ControllerServer) CreateSnapshotOnNS(nsProvider ns.ProviderInterface, volumePath, snapName string) (
    snapshot ns.Snapshot, err error) {

    l := s.log.WithField("func", "CreateSnapshotOnNS()")
    l.Infof("creating snapshot %+v@%+v", volumePath, snapName)

    //K8s doesn't allow to have same named snapshots for different volumes
    sourcePath := filepath.Dir(volumePath)

    existingSnapshots, err := nsProvider.GetSnapshots(sourcePath, true)
    if err != nil {
        return snapshot, status.Errorf(codes.NotFound, "Cannot get snapshots list: %s", err)
    }
    for _, s := range existingSnapshots {
        if s.Name == snapName && s.Parent != volumePath {
            return snapshot, status.Errorf(
                codes.AlreadyExists,
                "Snapshot '%s' already exists for filesystem: %s",
                snapName,
                s.Path,
            )
        }
    }

    snapshotPath := fmt.Sprintf("%s@%s", volumePath, snapName)

    // if here, than volumePath exists on some NS
    err = nsProvider.CreateSnapshot(ns.CreateSnapshotParams{
        Path: snapshotPath,
    })
    if err != nil && !ns.IsAlreadyExistNefError(err) {
        return snapshot, status.Errorf(codes.Internal, "Cannot create snapshot '%s': %s", snapshotPath, err)
    }

    snapshot, err = nsProvider.GetSnapshot(snapshotPath)
    if err != nil {
        return snapshot, status.Errorf(
            codes.Internal,
            "Snapshot '%s' has been created, but snapshot properties request failed: %s",
            snapshotPath,
            err,
        )
    }
    l.Infof("successfully created snapshot %+v@%+v", volumePath, snapName)
    return snapshot, nil
}

// DeleteSnapshot deletes snapshots
func (s *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (
    *csi.DeleteSnapshotResponse,
    error,
) {
    l := s.log.WithField("func", "DeleteSnapshot()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    err := s.refreshConfig("")
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }

    snapshotId := req.GetSnapshotId()
    if len(snapshotId) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Snapshot ID must be provided")
    }

    volume := ""
    snapshot := ""
    splittedString := strings.Split(snapshotId, "@")
    if len(splittedString) == 2 {
        volume = splittedString[0]
        snapshot = splittedString[1]
    } else {
        l.Infof("snapshot '%s' not found, that's OK for deletion request", snapshotId)
        return &csi.DeleteSnapshotResponse{}, nil
    }
    splittedVol := strings.Split(volume, ":")
    if len(splittedVol) != 2 {
        return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("VolumeId is in wrong format: %s", volume))
    }
    configName, volumePath := splittedVol[0], splittedVol[1]

    splittedPath := strings.Split(volumePath, "/")
    if len(splittedPath) != 3 {
        return nil, status.Errorf(codes.InvalidArgument, "Got wrong volume: %s", volume)
    }
    volumeGroup := strings.Join(splittedPath[:2], "/")
    params := ResolveNSParams{
        volumeGroup: volumeGroup,
        configName:  configName,
    }

    resolveResp, err := s.resolveNS(params)
    if err != nil {
        if status.Code(err) == codes.NotFound {
            l.Infof("snapshot '%s' not found, that's OK for deletion request", snapshotId)
            return &csi.DeleteSnapshotResponse{}, nil
        }
        return nil, err
    }
    nsProvider := resolveResp.nsProvider

    // if here, than snapshotPath exists on some NS
    snapshotPath := strings.Join([]string{volumePath, snapshot}, "@")
    err = nsProvider.DestroySnapshot(snapshotPath)
    if err != nil && !ns.IsNotExistNefError(err) {
        message := fmt.Sprintf("Failed to delete snapshot '%s'", snapshotPath)
        if ns.IsBusyNefError(err) {
            message += ", it has dependent filesystem"
        }
        return nil, status.Errorf(codes.Internal, "%s: %s", message, err)
    }

    l.Infof("snapshot '%s' has been deleted", snapshotPath)
    return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots returns the list of snapshots
func (s *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (
    *csi.ListSnapshotsResponse,
    error,
) {
    l := s.log.WithField("func", "ListSnapshots()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    err := s.refreshConfig("")
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }

    if req.GetSnapshotId() != "" {
        return s.getSnapshotListWithSingleSnapshot(req.GetSnapshotId(), req)
    } else if req.GetSourceVolumeId() != "" {
        return s.getVolumeSnapshotList(req.GetSourceVolumeId(), req)
    } else {
        response := csi.ListSnapshotsResponse{
            Entries: []*csi.ListSnapshotsResponse_Entry{},
        }
        for _, cfg := range s.config.NsMap {
            resp, _ := s.getVolumeSnapshotList(cfg.DefaultVolumeGroup, req)
            for _, snapshot := range resp.Entries {
                response.Entries = append(response.Entries, snapshot)
            }
            if len(resp.NextToken) > 0 {
                response.NextToken = resp.NextToken
                return &response, nil
            }
        }
        return &response, nil
    }
}

func (s *ControllerServer) getSnapshotListWithSingleSnapshot(snapshotId string, req *csi.ListSnapshotsRequest) (
    *csi.ListSnapshotsResponse,
    error,
) {
    l := s.log.WithField("func", "getSnapshotListWithSingleSnapshot()")
    l.Infof("Snapshot path: %s", snapshotId)

    response := csi.ListSnapshotsResponse{
        Entries: []*csi.ListSnapshotsResponse_Entry{},
    }

    splittedSnapshotPath := strings.Split(snapshotId, "@")
    if len(splittedSnapshotPath) != 2 {
        // bad snapshotID format, but it's ok, driver should return empty response
        l.Infof("Bad snapshot format: %s", splittedSnapshotPath)
        return &response, nil
    }
    splittedSnapshotId := strings.Split(splittedSnapshotPath[0], ":")
    configName, volumePath := splittedSnapshotId[0], splittedSnapshotId[1]

    splittedPath := strings.Split(volumePath, "/")
    if len(splittedPath) != 3 {
        return nil, status.Errorf(codes.InvalidArgument, "Got wrong volume: %s", volumePath)
    }
    volumeGroup := strings.Join(splittedPath[:2], "/")
    params := ResolveNSParams{
        volumeGroup: volumeGroup,
        configName:  configName,
    }

    resolveResp, err := s.resolveNS(params)
    if err != nil {
        if status.Code(err) == codes.NotFound {
            l.Infof("filesystem '%s' not found, that's OK for list request", volumePath)
            return &response, nil
        }
        return nil, err
    }
    nsProvider := resolveResp.nsProvider
    snapshotPath := fmt.Sprintf("%s@%s", volumePath, splittedSnapshotPath[1])
    snapshot, err := nsProvider.GetSnapshot(snapshotPath)
    if err != nil {
        if ns.IsNotExistNefError(err) {
            return &response, nil
        }
        return nil, status.Errorf(codes.Internal, "Cannot get snapshot '%s' for snapshot list: %s", snapshotPath, err)
    }
    response.Entries = append(response.Entries, convertNSSnapshotToCSISnapshot(snapshot, configName))
    l.Infof("snapshot '%s' found for '%s' filesystem", snapshot.Path, volumePath)
    return &response, nil
}

func (s *ControllerServer) getVolumeSnapshotList(volumeId string, req *csi.ListSnapshotsRequest) (
    *csi.ListSnapshotsResponse,
    error,
) {
    l := s.log.WithField("func", "getVolumeSnapshotList()")
    l.Infof("volume path: %s", volumeId)

    startingToken := req.GetStartingToken()
    maxEntries := req.GetMaxEntries()

    response := csi.ListSnapshotsResponse{
        Entries: []*csi.ListSnapshotsResponse_Entry{},
    }
    splittedVol := strings.Split(volumeId, ":")
    volumePath := ""
    configName := ""
    params := ResolveNSParams{}
    if len(splittedVol) == 2 {
        configName, volumePath = splittedVol[0], splittedVol[1]

        splittedPath := strings.Split(volumePath, "/")
        if len(splittedPath) != 3 {
            return nil, status.Errorf(codes.InvalidArgument, "Got wrong volume: %s", volumePath)
        }
        volumeGroup := strings.Join(splittedPath[:2], "/")
        params := ResolveNSParams{
            volumeGroup: volumeGroup,
            configName:  configName,
        }
        params.volumeGroup = volumeGroup
        params.configName = configName
    } else {
        volumePath = volumeId
        params.volumeGroup = volumePath
    }
    resolveResp, err := s.resolveNS(params)
    if err != nil {
        l.Infof("volume '%s' not found, that's OK for list request", volumePath)
        return &response, nil
    }

    nsProvider := resolveResp.nsProvider
    snapshots, err := nsProvider.GetSnapshots(volumePath, true)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "Cannot get snapshot list for '%s': %s", volumePath, err)
    }

    for i, snapshot := range snapshots {
        // skip all snapshots before startring token
        if snapshot.Path == startingToken {
            startingToken = ""
        }
        if startingToken != "" {
            continue
        }

        response.Entries = append(response.Entries, convertNSSnapshotToCSISnapshot(snapshot, resolveResp.configName))

        // if the requested maximum is reached (and specified) than set next token
        if maxEntries != 0 && int32(len(response.Entries)) == maxEntries {
            if i+1 < len(snapshots) { // next snapshots index exists
                l.Infof(
                    "max entries count (%d) has been reached while getting snapshots for '%s' filesystem, "+
                        "send response with next_token for pagination",
                    maxEntries,
                    volumePath,
                )
                response.NextToken = snapshots[i+1].Path
                return &response, nil
            }
        }
    }

    l.Infof("found %d snapshot(s) for %s filesystem", len(response.Entries), volumePath)

    return &response, nil
}

func convertNSSnapshotToCSISnapshot(snapshot ns.Snapshot, configName string) *csi.ListSnapshotsResponse_Entry {
    creationTime := timestamppb.New(snapshot.CreationTime)

    return &csi.ListSnapshotsResponse_Entry{
        Snapshot: &csi.Snapshot{
            SnapshotId:     fmt.Sprintf("%s:%s", configName, snapshot.Path),
            SourceVolumeId: fmt.Sprintf("%s:%s", configName, snapshot.Parent),
            CreationTime: creationTime,
            ReadyToUse: true, //TODO use actual state
            //SizeByte: 0 //TODO size of zero means it is unspecified
        },
    }
}

// ListVolumes - list volumes, shows only volumes created in defaultvolumeGroup
func (s *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (
    *csi.ListVolumesResponse,
    error,
) {
    l := s.log.WithField("func", "ListVolumes()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))
    startingToken := req.GetStartingToken()

    maxEntries := int(req.GetMaxEntries())
    if maxEntries < 0 {
        return nil, status.Errorf(codes.InvalidArgument, "req.MaxEntries must be 0 or greater, got: %d", maxEntries)
    }

    err := s.refreshConfig("")
    if err != nil {
        return nil, status.Errorf(codes.Aborted, "Cannot use config file: %s", err)
    }
    nextToken := ""
    entries := []*csi.ListVolumesResponse_Entry{}
    volumes := []ns.Volume{}
    for configName, _ := range s.config.NsMap {
        params := ResolveNSParams{
            configName: configName,
        }
        resolveResp, err := s.resolveNS(params)
        if err != nil {
            return nil, err
        }
        nsProvider := resolveResp.nsProvider
        volumeGroup := resolveResp.volumeGroup

        volumes, nextToken, err = nsProvider.GetVolumesWithStartingToken(
            volumeGroup,
            startingToken,
            maxEntries,
        )

        for _, item := range volumes {
            entries = append(entries, &csi.ListVolumesResponse_Entry{
                Volume: &csi.Volume{VolumeId: fmt.Sprintf("%s:%s", configName, item.Path)},
            })
        }
    }
        
    if startingToken != "" && len(entries) == 0 {
        return nil, status.Errorf(
            codes.Aborted,
            fmt.Sprintf("Failed to find filesystem started from token '%s': %s", startingToken, err),
        )
    }

    l.Infof("found %d entries(s)", len(entries))

    return &csi.ListVolumesResponse{
        Entries:   entries,
        NextToken: nextToken,
    }, nil
}

func (s *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (
    *csi.ControllerPublishVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "ControllerPublishVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    volCap := req.GetVolumeCapability()
    if volCap == nil {
        return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
    }

    volumeID := req.GetVolumeId()
    if len(volumeID) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
    }

    nodeID := req.GetNodeId()
    if len(nodeID) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Node ID not provided")
    }

    splittedVol := strings.Split(volumeID, ":")
    if len(splittedVol) != 2 {
        return nil, status.Error(codes.NotFound, fmt.Sprintf("VolumeId is in wrong format: %s", volumeID))
    }
    configName, volumePath := splittedVol[0], splittedVol[1]
    cfg := s.config.NsMap[configName]

    params := ResolveNSParams{
        volumeGroup: cfg.DefaultVolumeGroup,
        configName: configName,
    }
    response, err := s.resolveNS(params)
    nsProvider := response.nsProvider
    if err != nil {
        return nil, err
    }
    _, err = nsProvider.GetVolume(volumePath)
    if err != nil {
        l.Warnf("GetVolume ERROR: %+v", err)
        return nil, status.Errorf(codes.NotFound, "Volume %v not found on NexentaStor", volumePath)
    }

    if strings.Contains(nodeID, "fake-node") {
        return nil, status.Errorf(codes.NotFound, "Incorrect node: %v", nodeID)
    }

    // All attach operations are done in nodeStageVolume.
    return &csi.ControllerPublishVolumeResponse{}, nil
}

func (s *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (
    *csi.ControllerUnpublishVolumeResponse,
    error,
) {
    l := s.log.WithField("func", "ControllerUnpublishVolume()")
    l.Infof("request: '%+v'", protosanitizer.StripSecrets(req))

    var secret string
    secrets := req.GetSecrets()
    for _, v := range secrets {
        secret = v
    }
    err := s.refreshConfig(secret)
    if err != nil {
        return nil, status.Errorf(codes.FailedPrecondition, "Cannot use config file: %s", err)
    }

    volumeId := req.GetVolumeId()
    if len(volumeId) == 0 {
        return nil, status.Error(codes.InvalidArgument, "Volume ID must be provided")
    }
    splittedVol := strings.Split(volumeId, ":")
    if len(splittedVol) != 2 {
        l.Infof("Got wrong volumeId, but that is OK for deletion")
        return &csi.ControllerUnpublishVolumeResponse{}, nil
    }
    configName, volumePath := splittedVol[0], splittedVol[1]

    splittedPath := strings.Split(volumePath, "/")
    if len(splittedPath) != 3 {
        l.Infof("Got wrong volumeId, but that is OK for deletion")
        return &csi.ControllerUnpublishVolumeResponse{}, nil
    }
    volumeGroup := strings.Join(splittedPath[:2], "/")

    params := ResolveNSParams{
        volumeGroup: volumeGroup,
        configName: configName,
    }
    resolveResp, err := s.resolveNS(params)
    if err != nil {
        if status.Code(err) == codes.NotFound {
            l.Infof("volume '%s' not found, that's OK for deletion request", volumePath)
            return &csi.ControllerUnpublishVolumeResponse{}, nil
        }
        return nil, err
    }
    nsProvider := resolveResp.nsProvider

    lunMappingParams := ns.GetLunMappingsParams{
        Volume: volumePath,
    }
    luns, err := nsProvider.GetLunMappings(lunMappingParams)
    if err != nil {
        return nil, err
    }
    for _, lun := range luns {
        err = nsProvider.DestroyLunMapping(lun.Id)
        if err != nil{
            return nil, err
        }
    }

    luns, err = nsProvider.GetLunMappings(lunMappingParams)
    if err != nil {
        return nil, err
    }

    timeout := 60
    sleepTime := 2

    for len(luns) > 0 {
        if sleepTime > timeout {
            return nil, status.Errorf(codes.DeadlineExceeded, "Luns did not get deleted in %v seconds", sleepTime)
        }
        sleepTime = sleepTime + 1
        time.Sleep(time.Second * time.Duration(sleepTime))
        luns, err = nsProvider.GetLunMappings(lunMappingParams)
        if err != nil {
            return nil, err
        }
    }

    return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ControllerGetCapabilities - controller capabilities
func (s *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (
    *csi.ControllerGetCapabilitiesResponse,
    error,
) {
    s.log.WithField("func", "ControllerGetCapabilities()").Infof("request: '%+v'", req)

    var capabilities []*csi.ControllerServiceCapability
    for _, c := range supportedControllerCapabilities {
        capabilities = append(capabilities, newControllerServiceCapability(c))
    }
    return &csi.ControllerGetCapabilitiesResponse{
        Capabilities: capabilities,
    }, nil
}

func newControllerServiceCapability(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
    return &csi.ControllerServiceCapability{
        Type: &csi.ControllerServiceCapability_Rpc{
            Rpc: &csi.ControllerServiceCapability_RPC{
                Type: cap,
            },
        },
    }
}

// NewControllerServer - create an instance of controller service
func NewControllerServer(driver *Driver) (*ControllerServer, error) {
    l := driver.log.WithField("cmp", "ControllerServer")
    l.Info("create new ControllerServer...")
    resolverMap := make(map[string]ns.Resolver)

    for name, cfg := range driver.config.NsMap {
        nsResolver, err := ns.NewResolver(ns.ResolverArgs{
            Address:            cfg.Address,
            Username:           cfg.Username,
            Password:           cfg.Password,
            Log:                l,
            InsecureSkipVerify: *cfg.InsecureSkipVerify,
        })
        if err != nil {
            return nil, fmt.Errorf("Cannot create NexentaStor resolver: %s", err)
        }
        resolverMap[name] = *nsResolver
    }

    l.Info("Resolver map: %+v", resolverMap)
    return &ControllerServer{
        nsResolverMap: resolverMap,
        config:     driver.config,
        log:        l,
    }, nil
}
