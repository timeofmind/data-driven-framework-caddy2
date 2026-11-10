package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/timeofmind/data-driven-framework-go/cluster"
	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// ClusterConfig is the Caddyfile's cluster block: this node, every node,
// where the certificates are.
//
//	cluster {
//	    self n1
//	    listen 127.0.0.1:9101
//	    node n1 127.0.0.1:9101
//	    node n2 127.0.0.1:9102
//	    node n3 127.0.0.1:9103
//	    certs /etc/app/cluster      # ca.crt, <self>.crt, <self>.key (ddframework cluster-certs)
//	    replicas 3
//	}
type ClusterConfig struct {
	Self     string           `json:"self"`
	Listen   string           `json:"listen"`
	Nodes    []cluster.Member `json:"nodes"`
	CertsDir string           `json:"certs"`
	Replicas int              `json:"replicas,omitempty"`
}

func joinCluster(ctx context.Context, reg *runtime.Registry, cfg Config) error {
	c := cfg.Cluster
	read := func(name string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(c.CertsDir, name))
		if err != nil {
			return nil, fmt.Errorf("datadriven: cluster certs: %w", err)
		}
		return b, nil
	}
	caCert, err := read("ca.crt")
	if err != nil {
		return err
	}
	cert, err := read(c.Self + ".crt")
	if err != nil {
		return err
	}
	key, err := read(c.Self + ".key")
	if err != nil {
		return err
	}
	_, err = reg.JoinCluster(ctx, runtime.ClusterConfig{Self: c.Self, Listen: c.Listen, Members: c.Nodes,
		CACert: caCert, Cert: cert, Key: key, Replicas: c.Replicas, Logger: cfg.Logger})
	return err
}
