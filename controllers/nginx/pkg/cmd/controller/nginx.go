/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api"

	"k8s.io/ingress/core/pkg/ingress"
	"k8s.io/ingress/core/pkg/ingress/defaults"

	"errors"

	"k8s.io/ingress/controllers/nginx/pkg/config"
	ngx_template "k8s.io/ingress/controllers/nginx/pkg/template"
	"k8s.io/ingress/controllers/nginx/pkg/version"
)

var (
	tmplPath = "/etc/nginx/template/nginx.tmpl"
	cfgPath  = "/etc/nginx/nginx.conf"
	binary   = "/usr/sbin/nginx"
)

// newNGINXController creates a new NGINX Ingress controller.
// If the environment variable NGINX_BINARY exists it will be used
// as source for nginx commands
func newNGINXController() ingress.Controller {
	ngx := os.Getenv("NGINX_BINARY")
	if ngx == "" {
		ngx = binary
	}
	n := NGINXController{binary: ngx}

	var onChange func()
	onChange = func() {
		template, err := ngx_template.NewTemplate(tmplPath, onChange)
		if err != nil {
			// this error is different from the rest because it must be clear why nginx is not working
			glog.Errorf(`
-------------------------------------------------------------------------------
Error loading new template : %v
-------------------------------------------------------------------------------
`, err)
			return
		}

		n.t.Close()
		n.t = template
		glog.Info("new NGINX template loaded")
	}

	ngxTpl, err := ngx_template.NewTemplate(tmplPath, onChange)
	if err != nil {
		glog.Fatalf("invalid NGINX template: %v", err)
	}

	n.t = ngxTpl
	go n.Start()

	return n
}

// NGINXController ...
type NGINXController struct {
	t *ngx_template.Template

	binary string
}

// Start start a new NGINX master process running in foreground.
func (n NGINXController) Start() {
	glog.Info("starting NGINX process...")
	cmd := exec.Command(n.binary, "-c", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		glog.Fatalf("nginx error: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		glog.Errorf("nginx error: %v", err)
	}
}

// Reload checks if the running configuration file is different
// to the specified and reload nginx if required
func (n NGINXController) Reload(data []byte) ([]byte, error) {
	if !n.isReloadRequired(data) {
		return nil, fmt.Errorf("Reload not required")
	}

	err := ioutil.WriteFile(cfgPath, data, 0644)
	if err != nil {
		return nil, err
	}

	return exec.Command(n.binary, "-s", "reload").CombinedOutput()
}

// Test checks is a file contains a valid NGINX configuration
func (n NGINXController) Test(file string) *exec.Cmd {
	return exec.Command(n.binary, "-t", "-c", file)
}

// BackendDefaults returns the nginx defaults
func (n NGINXController) BackendDefaults() defaults.Backend {
	d := config.NewDefault()
	return d.Backend
}

// IsReloadRequired check if the new configuration file is different
// from the current one.
func (n NGINXController) isReloadRequired(data []byte) bool {
	in, err := os.Open(cfgPath)
	if err != nil {
		return false
	}
	src, err := ioutil.ReadAll(in)
	in.Close()
	if err != nil {
		return false
	}

	if !bytes.Equal(src, data) {
		tmpfile, err := ioutil.TempFile("", "nginx-cfg-diff")
		if err != nil {
			glog.Errorf("error creating temporal file: %s", err)
			return false
		}
		defer tmpfile.Close()
		err = ioutil.WriteFile(tmpfile.Name(), data, 0644)
		if err != nil {
			return false
		}

		diffOutput, err := diff(src, data)
		if err != nil {
			glog.Errorf("error computing diff: %s", err)
			return true
		}

		if glog.V(2) {
			glog.Infof("NGINX configuration diff\n")
			glog.Infof("%v", string(diffOutput))
		}
		return len(diffOutput) > 0
	}
	return false
}

// Info return build information
func (n NGINXController) Info() *ingress.BackendInfo {
	return &ingress.BackendInfo{
		Name:       "NGINX",
		Release:    version.RELEASE,
		Build:      version.COMMIT,
		Repository: version.REPO,
	}
}

// testTemplate checks if the NGINX configuration inside the byte array is valid
// running the command "nginx -t" using a temporal file.
func (n NGINXController) testTemplate(cfg []byte) error {
	tmpfile, err := ioutil.TempFile("", "nginx-cfg")
	if err != nil {
		return err
	}
	defer tmpfile.Close()
	ioutil.WriteFile(tmpfile.Name(), cfg, 0644)
	out, err := n.Test(tmpfile.Name()).CombinedOutput()
	if err != nil {
		// this error is different from the rest because it must be clear why nginx is not working
		oe := fmt.Sprintf(`
-------------------------------------------------------------------------------
Error: %v
%v
-------------------------------------------------------------------------------
`, err, string(out))
		return errors.New(oe)
	}

	os.Remove(tmpfile.Name())
	return nil
}

// OnUpdate is called by syncQueue in https://github.com/aledbf/ingress-controller/blob/master/pkg/ingress/controller/controller.go#L82
// periodically to keep the configuration in sync.
//
// convert configmap to custom configuration object (different in each implementation)
// write the custom template (the complexity depends on the implementation)
// write the configuration file
// returning nill implies the backend will be reloaded.
// if an error is returned means requeue the update
func (n NGINXController) OnUpdate(cmap *api.ConfigMap, ingressCfg ingress.Configuration) ([]byte, error) {
	var longestName int
	var serverNames int
	for _, srv := range ingressCfg.Servers {
		serverNames += len([]byte(srv.Hostname))
		if longestName < len(srv.Hostname) {
			longestName = len(srv.Hostname)
		}
	}

	cfg := ngx_template.ReadConfig(cmap)

	// NGINX cannot resize the has tables used to store server names.
	// For this reason we check if the defined size defined is correct
	// for the FQDN defined in the ingress rules adjusting the value
	// if is required.
	// https://trac.nginx.org/nginx/ticket/352
	// https://trac.nginx.org/nginx/ticket/631
	nameHashBucketSize := nextPowerOf2(longestName)
	if nameHashBucketSize > cfg.ServerNameHashBucketSize {
		glog.V(3).Infof("adjusting ServerNameHashBucketSize variable from %v to %v",
			cfg.ServerNameHashBucketSize, nameHashBucketSize)
		cfg.ServerNameHashBucketSize = nameHashBucketSize
	}
	serverNameHashMaxSize := nextPowerOf2(serverNames)
	if serverNameHashMaxSize > cfg.ServerNameHashMaxSize {
		glog.V(3).Infof("adjusting ServerNameHashMaxSize variable from %v to %v",
			cfg.ServerNameHashMaxSize, serverNameHashMaxSize)
		cfg.ServerNameHashMaxSize = serverNameHashMaxSize
	}

	return n.t.Write(config.TemplateConfig{
		BacklogSize:        sysctlSomaxconn(),
		Backends:           ingressCfg.Backends,
		PassthrougBackends: ingressCfg.PassthroughBackends,
		Servers:            ingressCfg.Servers,
		TCPBackends:        ingressCfg.TCPEndpoints,
		UDPBackends:        ingressCfg.UPDEndpoints,
		HealthzURI:         "/healthz",
		CustomErrors:       len(cfg.CustomHTTPErrors) > 0,
		Cfg:                cfg,
	}, n.testTemplate)
}

// http://graphics.stanford.edu/~seander/bithacks.html#RoundUpPowerOf2
// https://play.golang.org/p/TVSyCcdxUh
func nextPowerOf2(v int) int {
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v++

	return v
}
