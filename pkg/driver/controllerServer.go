package driver

import (
    "fmt"
    "path/filepath"
    "strings"
    // "strconv"

    "github.com/container-storage-interface/spec/lib/go/csi"
    // timestamp "github.com/golang/protobuf/ptypes/timestamp"
    "github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
    "github.com/sirupsen/logrus"
    "golang.org/x/net/context"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"

    "github.com/Nexenta/go-nexentastor/pkg/ns"
    "github.com/Nexenta/nexentastor-csi-driver-block/pkg/config"
)

const TopologyKeyZone = "topology.kubernetes.io/zone"

// supportedControllerCapabilities - driver controller capabilities
var supportedControllerCapabilities = []csi.ControllerServiceCapability_RPC_Type{
    csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
    csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
    // csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
    // csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
    // csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
    csi.ControllerServiceCapability_RPC_GET_CAPACITY,
    // csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
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
                InsecureSkipVerify: true, //TODO move to config
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
    // No zone -> pick NS for given dataset and configName. TODO: load balancing
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
            if params.volumeGroup == "" {
                volumeGroup = s.config.NsMap[name].DefaultVolumeGroup
            } else {
                volumeGroup = params.volumeGroup
            }
            if params.zone == s.config.NsMap[name].Zone {
                nsProvider, err = resolver.ResolveFromVg(volumeGroup)
                if nsProvider != nil {
                    l.Infof("Found dataset %s on NexentaStor [%s]", volumeGroup, name)
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
    // block is not supported
    if requestedVolumeCapability.GetBlock() != nil {
        return false
    }

    requestedMode := requestedVolumeCapability.GetAccessMode().GetMode()
    for _, volumeCapability := range supportedVolumeCapabilities {
        if volumeCapability.GetAccessMode().GetMode() == requestedMode {
            return true
        }
    }
    return false
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

    // get dataset path from runtime params, set default if not specified
    volumeGroup := ""
    if v, ok := reqParams["dataset"]; ok {
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
    filesystem, err := nsProvider.GetFilesystem(resolveResp.volumeGroup)
    if err != nil {
        return nil, err
    }

    availableCapacity := filesystem.GetReferencedQuotaSize()
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

    // get dataset path from runtime params, set default if not specified
    volumeGroup := ""
    if v, ok := reqParams["volumeGroup"]; ok {
        volumeGroup = v
    }
    configName := ""
    if v, ok := reqParams["configName"]; ok {
        configName = v
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
    resolveResp, err := s.resolveNS(params)
    if err != nil {
        return nil, err
    }
    nsProvider := resolveResp.nsProvider
    volumeGroup = resolveResp.volumeGroup
    volumePath := filepath.Join(volumeGroup, volumeName)
    err = s.createNewVolume(nsProvider, volumePath, capacityBytes)
    if err != nil {
        return nil, err
    }
    res = &csi.CreateVolumeResponse{
        Volume: &csi.Volume{
            // ContentSource: contentSource,
            VolumeId:      fmt.Sprintf("%s:%s", resolveResp.configName, volumePath),
            CapacityBytes: capacityBytes,
            VolumeContext: map[string]string{
                "dataIp":       reqParams["dataIp"],
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
) (error) {
    l := s.log.WithField("func", "createNewVolume()")
    l.Infof("nsProvider: %s, volumePath: %s", nsProvider, volumePath)

    err := nsProvider.CreateVolume(ns.CreateVolumeParams{
        Path:                volumePath,
        VolumeSize:          capacityBytes,
    })

    if err != nil {
        if ns.IsAlreadyExistNefError(err) {
            // existingFilesystem, err := nsProvider.GetFilesystem(volumePath)
            // if err != nil {
            //     return status.Errorf(
            //         codes.Internal,
            //         "Volume '%s' already exists, but volume properties request failed: %s",
            //         volumePath,
            //         err,
            //     )
            // } else if capacityBytes != 0 && existingFilesystem.GetReferencedQuotaSize() != capacityBytes {
            //     return status.Errorf(
            //         codes.AlreadyExists,
            //         "Volume '%s' already exists, but with a different size: requested=%d, existing=%d",
            //         volumePath,
            //         capacityBytes,
            //         existingFilesystem.GetReferencedQuotaSize(),
            //     )
            // }

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
    if len(splittedPath) != 2 {
        l.Infof("Got wrong volumeId, but that is OK for deletion")
        return &csi.DeleteVolumeResponse{}, nil
    }
    volumeGroup := splittedPath[0]

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
    s.log.WithField("func", "CreateSnapshot()").Warnf("request: '%+v' - not implemented", req)
    return nil, status.Error(codes.Unimplemented, "")
}

// DeleteSnapshot deletes snapshots
func (s *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (
    *csi.DeleteSnapshotResponse,
    error,
) {
    s.log.WithField("func", "DeleteSnapshot()").Warnf("request: '%+v' - not implemented", req)
    return nil, status.Error(codes.Unimplemented, "")
}

// ListSnapshots returns the list of snapshots
func (s *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (
    *csi.ListSnapshotsResponse,
    error,
) {
    s.log.WithField("func", "ListSnapshots()").Warnf("request: '%+v' - not implemented", req)
    return nil, status.Error(codes.Unimplemented, "")
}

// ListVolumes - list volumes, shows only volumes created in defaultDataset
func (s *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (
    *csi.ListVolumesResponse,
    error,
) {
    s.log.WithField("func", "ListVolumes()").Warnf("request: '%+v' - not implemented", req)
    return nil, status.Error(codes.Unimplemented, "")
}

// ControllerPublishVolume - not supported
func (s *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (
    *csi.ControllerPublishVolumeResponse,
    error,
) {
    s.log.WithField("func", "ControllerPublishVolume()").Warnf("request: '%+v' - not implemented", req)
    return nil, status.Error(codes.Unimplemented, "")
}

// ControllerUnpublishVolume - not supported
func (s *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (
    *csi.ControllerUnpublishVolumeResponse,
    error,
) {
    s.log.WithField("func", "ControllerUnpublishVolume()").Warnf("request: '%+v' - not implemented", req)
    return nil, status.Error(codes.Unimplemented, "")
}

// ControllerExpandVolume - not supported
func (s *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (
    *csi.ControllerExpandVolumeResponse,
    error,
) {
    s.log.WithField("func", "ControllerExpandVolume()").Warnf("request: '%+v' - not implemented", req)
    return nil, status.Error(codes.Unimplemented, "")
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
            InsecureSkipVerify: true, //TODO move to config
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
