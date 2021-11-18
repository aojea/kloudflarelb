package cloudflared

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aojea/kloudflarelb/pkg/config"

	cloudflaredconfig "github.com/cloudflare/cloudflared/config"
	"github.com/pkg/errors"
	yaml "gopkg.in/yaml.v2"

	"k8s.io/klog/v2"
)

// Configuration file
// https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/configuration/configuration-file
const defaultFilename = "config.yaml"

// https://github.com/cloudflare/cloudflared/blob/master/config/configuration.go
type Configuration struct {
	cloudflaredconfig.Configuration
	path string
}

func (c *Configuration) AddIngress(hostname, service string) {
	c.Ingress = append(c.Ingress, cloudflaredconfig.UnvalidatedIngressRule{
		Hostname: hostname,
		Service:  service,
	})
}

// Write atomically the configuration file if is different
func (c *Configuration) Write() error {
	if c.path == "" {
		return fmt.Errorf("missing configuration file name")
	}

	if err := os.MkdirAll(filepath.Dir(c.path), os.ModePerm); err != nil {
		return err
	}

	tempFile, err := ioutil.TempFile("", "klb")
	if err != nil {
		return err
	}
	defer tempFile.Close()

	tempname := tempFile.Name()
	defer os.Remove(tempname)

	if err := yaml.NewEncoder(tempFile).Encode(&c); err != nil {
		return err
	}

	if sameFile(tempname, c.path) {
		return nil
	}

	err = os.Rename(tempname, c.path)
	if err != nil {
		return err
	}

	return nil
}

func (c *Configuration) Run(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "cloudflared", "--no-autoupdate", "--config", defaultFilename)
	go func() {
		err := cmd.Run()
		if err != nil {
			klog.Errorf("Error running cloudflared: %w", err)
		}
	}()
}

func NewFromFile(path string) (*Configuration, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := Configuration{
		path: path,
	}
	if err := yaml.NewDecoder(f).Decode(&c); err != nil {
		if err == io.EOF {
			klog.Errorf("Configuration file %s was empty", path)
			return &c, nil
		}
		return nil, errors.Wrap(err, "error parsing YAML in config file at "+c.path)
	}
	return &c, nil
}

func NewFromConfig(config config.Config) *Configuration {
	c := Configuration{
		path: defaultFilename,
	}
	c.TunnelID = config.TunnelID
	return &c
}

func sameFile(path1, path2 string) bool {
	ia1, err := os.Stat(path1)
	if err != nil {
		return false
	}
	ia2, err := os.Stat(path2)
	if err != nil {
		return false
	}
	return os.SameFile(ia1, ia2)
}
