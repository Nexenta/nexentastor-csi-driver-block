package driver_test

import (
	"flag"
	"fmt"
	"os"
	"testing"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/educlos/testrail"
	"github.com/sirupsen/logrus"

	"github.com/Nexenta/nexentastor-csi-driver-block/tests/utils/k8s"
	"github.com/Nexenta/nexentastor-csi-driver-block/tests/utils/remote"
)

const (
	defaultSecretName = "nexentastor-csi-driver-block-config"
)

type config struct {
	k8sConnectionString string
	k8sDeploymentFile   string
	k8sSecretFile       string
	k8sSecretName       string
}

var c *config
var l *logrus.Entry
var fsTypeFlag string
var fsType string
var username = os.Getenv("TESTRAIL_USR")
var password = os.Getenv("TESTRAIL_PSWD")
var url = os.Getenv("TESTRAIL_URL")
var testResult testrail.SendableResult

func TestMain(m *testing.M) {
	var (
		k8sConnectionString = flag.String("k8sConnectionString", "", "K8s connection string [user@host]")
		k8sDeploymentFile   = flag.String("k8sDeploymentFile", "", "path to driver deployment yaml file")
		k8sSecretFile       = flag.String("k8sSecretFile", "", "path to yaml driver config file (for k8s secret)")
		k8sSecretName       = flag.String("k8sSecretName", defaultSecretName, "k8s secret name")
		fsTypeFlag          = flag.String("fsTypeFlag", "", "FS type for tests (nfs/cifs/block)")
	)

	flag.Parse()

	if *k8sConnectionString == "" {
		fmt.Println("Parameter '--k8sConnectionString' is missed")
		os.Exit(1)
	} else if *k8sDeploymentFile == "" {
		fmt.Println("Parameter '--k8sDeploymentFile' is missed")
		os.Exit(1)
	} else if *k8sSecretFile == "" {
		fmt.Println("Parameter '--k8sSecretFile' is missed")
		os.Exit(1)
	}

	c = &config{
		k8sConnectionString: *k8sConnectionString,
		k8sDeploymentFile:   *k8sDeploymentFile,
		k8sSecretFile:       *k8sSecretFile,
		k8sSecretName:       *k8sSecretName,
	}

	fsType = *fsTypeFlag

	// init logger
	l = logrus.New().WithField("title", "tests")

	noColors := false
	if v := os.Getenv("NOCOLORS"); v != "" && v != "false" {
		noColors = true
	}

	// logger formatter
	l.Logger.SetFormatter(&nested.Formatter{
		HideKeys:    true,
		FieldsOrder: []string{"title", "address", "cmp", "name", "func"},
		NoColors:    noColors,
	})

	l.Info("run...")
	l.Info("Config:")
	l.Infof(" - k8s server:    %s", c.k8sConnectionString)
	l.Infof(" - driver yaml:   %s", c.k8sDeploymentFile)
	l.Infof(" - driver config: %s", c.k8sSecretFile)
	l.Infof(" - secret name:   %s", c.k8sSecretName)

	os.Exit(m.Run())
}

func TestDriver_deploy(t *testing.T) {
	// connect to TestRail
	client := testrail.NewClient(url, username, password)

	rc, err := remote.NewClient(c.k8sConnectionString, l)
	if err != nil {
		t.Errorf("Cannot create connection: %s", err)
		return
	}

	out, err := rc.Exec("kubectl version")
	if err != nil {
		t.Errorf("cannot get kubectl version: %s; out: %s", err, out)
		return
	}
	t.Logf("kubectl version:\n%s", out)
	l.Infof("kubectl version:\n%s", out)

	k8sDriver, err := k8s.NewDeployment(k8s.DeploymentArgs{
		RemoteClient: rc,
		ConfigFile:   c.k8sDeploymentFile,
		SecretFile:   c.k8sSecretFile,
		SecretName:   c.k8sSecretName,
		Log:          l,
	})
	defer k8sDriver.CleanUp()
	defer k8sDriver.Delete(nil)
	if err != nil {
		t.Errorf("Cannot create K8s deployment: %s", err)
		return
	}

	installed := t.Run("install driver", func(t *testing.T) {
		t.Log("create k8s secret for driver")
		k8sDriver.DeleteSecret()
		if err := k8sDriver.CreateSecret(); err != nil {
			t.Fatal(err)
		}

		waitPods := []string{
			"nexentastor-block-csi-controller-.*Running",
			"nexentastor-block-csi-node-.*Running",
		}

		t.Log("instal the driver")
		if err := k8sDriver.Apply(waitPods); err != nil {
			t.Fatal(err)
		}

		testResult.StatusID = 1
		testResult.Comment = "Installation - success"
		if _, err := client.AddResultForCase(5151, 801258, testResult); err != nil {
			l.Warn("Can't add test result to TestRail")
		}
		t.Log("done.")
	})
	if !installed {
		testResult.StatusID = 5
		testResult.Comment = "Installation - failed"
		if _, err := client.AddResultForCase(5151, 801258, testResult); err != nil {
			l.Warn("Can't add test result to TestRail")
		}
		t.Fatal()
	}

	t.Run("deploy nginx pod with dynamic block volume provisioning", func(t *testing.T) {
		nginxPodName := "nginx-dynamic-volume"
		testResult.StatusID = 5
		testResult.Comment = "Create Pod and Mount Volume - failed"

		getNginxRunCommand := func(cmd string) string {
			return fmt.Sprintf("kubectl exec -c nginx %s -- /bin/bash -c \"%s\"", nginxPodName, cmd)
		}

		k8sNginx, err := k8s.NewDeployment(k8s.DeploymentArgs{
			RemoteClient: rc,
			ConfigFile:   "../../examples/kubernetes/nginx-dynamic-volume.yaml",
			Log:          l,
		})
		defer k8sNginx.CleanUp()
		defer k8sNginx.Delete(nil)
		if err != nil {
			if _, err := client.AddResultForCase(5151, 801262, testResult); err != nil {
				l.Warn("Can't add test result to TestRail")
			}
			t.Fatalf("Cannot create K8s nginx deployment: %s", err)
		}

		t.Log("deploy nginx container with read-write volume")
		if err := k8sNginx.Apply([]string{nginxPodName + ".*Running"}); err != nil {
			if _, err := client.AddResultForCase(5151, 801262, testResult); err != nil {
				l.Warn("Can't add test result to TestRail")
			}
			t.Fatal(err)
		}

		t.Log("write data to the volume")
		if _, err := rc.Exec(getNginxRunCommand("echo 'test' > /usr/share/nginx/html/data.txt")); err != nil {
			if _, err := client.AddResultForCase(5151, 801262, testResult); err != nil {
				l.Warn("Can't add test result to TestRail")
			}
			t.Fatal(fmt.Errorf("Cannot write data to nginx volume: %s", err))
		}

		t.Log("check if the data has been written to the volume")
		if _, err := rc.Exec(getNginxRunCommand("grep 'test' /usr/share/nginx/html/data.txt")); err != nil {
			if _, err := client.AddResultForCase(5151, 801262, testResult); err != nil {
				l.Warn("Can't add test result to TestRail")
			}
			t.Fatal(fmt.Errorf("Data hasn't been written to nginx container: %s", err))
		}

		t.Log("delete the nginx container with read-write volume")
		if err := k8sNginx.Delete([]string{nginxPodName}); err != nil {
			if _, err := client.AddResultForCase(5151, 801262, testResult); err != nil {
				l.Warn("Can't add test result to TestRail")
			}
			t.Fatal(err)
		}

		testResult.StatusID = 1
		testResult.Comment = "Create Pod and Mount Volume - success"
		if _, err := client.AddResultForCase(5151, 801262, testResult); err != nil {
			l.Warn("Can't add test result to TestRail")
		}

		t.Log("done.")
	})

	//
	// for future tests
	//

	t.Run("uninstall driver", func(t *testing.T) {
		t.Log("deleting the driver")
		if err := k8sDriver.Delete([]string{"nexentastor-block-csi-.*"}); err != nil {
			testResult.StatusID = 5
			testResult.Comment = "Uninstallation - failed"
			if _, err := client.AddResultForCase(5151, 801261, testResult); err != nil {
				l.Warn("Can't add test result to TestRail")
			}
			t.Fatal(err)
		}

		t.Log("deleting the secret")
		if err := k8sDriver.DeleteSecret(); err != nil {
			t.Fatal(err)
		}

		testResult.StatusID = 1
		testResult.Comment = "Uninstallation - success"
		if _, err := client.AddResultForCase(5151, 801261, testResult); err != nil {
			l.Warn("Can't add test result to TestRail")
		}

		t.Log("done.")
	})
}
