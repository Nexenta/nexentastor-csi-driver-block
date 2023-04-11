module github.com/Nexenta/nexentastor-csi-driver-block

go 1.20

require (
	github.com/Nexenta/go-nexentastor v2.7.1+incompatible
	github.com/antonfisher/nested-logrus-formatter v1.1.0
	github.com/cenkalti/backoff v2.2.1+incompatible
	github.com/container-storage-interface/spec v1.7.0
	github.com/educlos/testrail v0.0.0-20200402224751-3ab3c62b1fdc
	github.com/google/uuid v1.1.2
	github.com/kubernetes-csi/csi-lib-utils v0.7.0
	github.com/sirupsen/logrus v1.8.1
	golang.org/x/net v0.8.0
	golang.org/x/sys v0.6.0
	google.golang.org/grpc v1.49.0
	google.golang.org/protobuf v1.28.1
	gopkg.in/yaml.v2 v2.4.0
	k8s.io/mount-utils v0.0.0
	k8s.io/utils v0.0.0-20221107191617-1a15be271d1d
)

require (
	github.com/go-logr/logr v1.2.3 // indirect
	github.com/golang/protobuf v1.5.2 // indirect
	github.com/google/go-cmp v0.5.9 // indirect
	github.com/stretchr/testify v1.8.0 // indirect
	golang.org/x/text v0.8.0 // indirect
	google.golang.org/genproto v0.0.0-20220502173005-c8bf987b8c21 // indirect
	k8s.io/klog/v2 v2.80.1 // indirect
)

replace (
	k8s.io/api => k8s.io/api v0.17.3
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.17.3
	k8s.io/apimachinery => k8s.io/apimachinery v0.17.4-beta.0
	k8s.io/apiserver => k8s.io/apiserver v0.17.3
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.17.3
	k8s.io/client-go => k8s.io/client-go v0.17.3
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.17.3
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.17.3
	k8s.io/code-generator => k8s.io/code-generator v0.17.4-beta.0
	k8s.io/component-base => k8s.io/component-base v0.17.3
	k8s.io/component-helpers => k8s.io/component-helpers v0.21.0
	k8s.io/controller-manager => k8s.io/controller-manager v0.21.0
	k8s.io/cri-api => k8s.io/cri-api v0.17.4-beta.0
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.17.3
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.17.3
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.17.3
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.17.3
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.17.3
	k8s.io/kubectl => k8s.io/kubectl v0.17.3
	k8s.io/kubelet => k8s.io/kubelet v0.17.3
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.17.3
	k8s.io/metrics => k8s.io/metrics v0.17.3
	k8s.io/mount-utils => k8s.io/mount-utils v0.21.0-beta.0
	k8s.io/node-api => k8s.io/node-api v0.17.3
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.17.3
	k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.17.3
	k8s.io/sample-controller => k8s.io/sample-controller v0.17.3
)
