/*
Copyright 2022 Huawei Cloud Computing Technologies Co., Ltd.

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

package config

import (
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"time"

	"github.com/influxdata/influxdb/toml"
	"github.com/openGemini/openGemini/lib/errno"
)

type Spdy struct {
	ByteBufferPoolDefaultSize uint64

	RecvWindowSize          int `toml:"recv-window-size"`
	ConcurrentAcceptSession int `toml:"concurrent-accept-session"`
	ConnPoolSize            int `toml:"conn-pool-size"`

	OpenSessionTimeout   toml.Duration `toml:"open-session-timeout"`
	SessionSelectTimeout toml.Duration `toml:"session-select-timeout"`
	TCPDialTimeout       toml.Duration `toml:"tcp-dial-timeout"`
	DataAckTimeout       toml.Duration `toml:"data-ack-timeout"`

	TLSEnable             bool   `toml:"tls-enable"`
	TLSClientAuth         bool   `toml:"tls-client-auth"`
	TLSInsecureSkipVerify bool   `toml:"tls-insecure-skip-verify"`
	TLSCertificate        string `toml:"tls-certificate"`
	TLSPrivateKey         string `toml:"tls-private-key"`
	TLSClientCertificate  string `toml:"tls-client-certificate"`
	TLSClientPrivateKey   string `toml:"tls-client-private-key"`
	TLSCARoot             string `toml:"tls-ca-root"`
	TLSServerName         string `toml:"tls-server-name"`
}

const (
	Second = toml.Duration(time.Second)

	MinRecvWindowSize          = 2
	MinConcurrentAcceptSession = 1024
	MinOpenSessionTimeout      = 60 * Second
	MinSessionSelectTimeout    = 60 * Second
	MinTCPDialTimeout          = Second
	MinConnPoolSize            = 2

	DefaultRecvWindowSize          = 8
	DefaultConcurrentAcceptSession = 4096
	DefaultOpenSessionTimeout      = 300 * Second
	DefaultSessionSelectTimeout    = 300 * Second
	DefaultTCPDialTimeout          = Second
	DefaultConnPoolSize            = 4
)

func NewSpdy() Spdy {
	return Spdy{
		RecvWindowSize:            DefaultRecvWindowSize,
		ByteBufferPoolDefaultSize: 0,
		ConcurrentAcceptSession:   DefaultConcurrentAcceptSession,
		OpenSessionTimeout:        DefaultOpenSessionTimeout,
		SessionSelectTimeout:      DefaultSessionSelectTimeout,
		TCPDialTimeout:            DefaultTCPDialTimeout,
		TLSEnable:                 false,
		ConnPoolSize:              DefaultConnPoolSize,
	}
}

func (c *Spdy) ApplyEnvOverrides(_ func(string) string) error {
	return nil
}

func (c *Spdy) GetOpenSessionTimeout() time.Duration {
	return time.Duration(c.OpenSessionTimeout)
}

func (c *Spdy) GetSessionSelectTimeout() time.Duration {
	return time.Duration(c.SessionSelectTimeout)
}

func (c *Spdy) GetTCPDialTimeout() time.Duration {
	return time.Duration(c.TCPDialTimeout)
}

func (c *Spdy) NewTLSConfig() (*tls.Config, error) {
	conf, err := c.newTLSConfig(c.TLSClientCertificate, c.TLSClientPrivateKey)
	if err != nil {
		return nil, err
	}

	if c.TLSClientAuth {
		pool := x509.NewCertPool()
		pem, err := ioutil.ReadFile(c.TLSCARoot)
		if err != nil {
			return nil, err
		}
		pool.AppendCertsFromPEM(pem)
		conf.ClientAuth = tls.RequireAndVerifyClientCert
		conf.ClientCAs = pool
	}

	return conf, nil
}

func (c *Spdy) NewClientTLSConfig() (*tls.Config, error) {
	return c.newTLSConfig(c.TLSClientCertificate, c.TLSClientPrivateKey)
}

func (c Spdy) Validate() error {
	if !c.TLSEnable {
		return nil
	}

	cv := NewCertValidator(c.TLSCertificate, c.TLSPrivateKey)
	return cv.Validate()
}

func (c *Spdy) newTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	if !c.TLSEnable {
		return nil, errno.NewError(errno.InvalidTLSConfig)
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	// #nosec
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		ServerName:         c.TLSServerName,
		InsecureSkipVerify: c.TLSInsecureSkipVerify,
		MinVersion:         tls.VersionTLS13,
	}, nil
}

func FormatSpdy(cfg *Spdy) {
	cfg.RecvWindowSize = formatInt(cfg.RecvWindowSize, MinRecvWindowSize, DefaultRecvWindowSize)
	cfg.ConcurrentAcceptSession = formatInt(cfg.ConcurrentAcceptSession, MinConcurrentAcceptSession, DefaultConcurrentAcceptSession)
	cfg.OpenSessionTimeout = limitDuration(cfg.OpenSessionTimeout, MinOpenSessionTimeout, DefaultOpenSessionTimeout)
	cfg.SessionSelectTimeout = limitDuration(cfg.SessionSelectTimeout, MinSessionSelectTimeout, DefaultSessionSelectTimeout)
	cfg.TCPDialTimeout = limitDuration(cfg.TCPDialTimeout, MinTCPDialTimeout, DefaultTCPDialTimeout)
	cfg.ConnPoolSize = formatInt(cfg.ConnPoolSize, MinConnPoolSize, DefaultConnPoolSize)
	if cfg.TLSCertificate == "" {
		cfg.TLSEnable = false
	}
	if cfg.TLSPrivateKey == "" {
		cfg.TLSPrivateKey = cfg.TLSCertificate
	}
	if cfg.TLSClientPrivateKey == "" {
		cfg.TLSClientPrivateKey = cfg.TLSClientCertificate
	}
	if cfg.TLSClientCertificate == "" || cfg.TLSCARoot == "" {
		cfg.TLSClientAuth = false
	}
	if !cfg.TLSClientAuth {
		cfg.TLSClientCertificate = cfg.TLSCertificate
	}
}

func formatInt(got int, min int, def int) int {
	if got <= 0 {
		return def
	}
	if got < min {
		return min
	}
	return got
}

func limitDuration(got toml.Duration, min toml.Duration, def toml.Duration) toml.Duration {
	if got <= 0 {
		return def
	}
	if got < min {
		return min
	}
	return got
}
