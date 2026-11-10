package deploytest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// cHooksFile is the host-Traefik dynamic config shape this repo ships. Decoding
// is strict (decodeStrict), so a key added to the route file but not modelled
// here fails the test instead of slipping through.
type cHooksFile struct {
	HTTP cHTTP `yaml:"http"`
}

type cHTTP struct {
	Routers  map[string]cRouter  `yaml:"routers"`
	Services map[string]cService `yaml:"services"`
}

type cRouter struct {
	Rule        string   `yaml:"rule"`
	EntryPoints []string `yaml:"entryPoints"`
	Service     string   `yaml:"service"`
	TLS         cTLS     `yaml:"tls"`
}

type cTLS struct {
	CertResolver string `yaml:"certResolver"`
}

type cService struct {
	LoadBalancer cLoadBalancer `yaml:"loadBalancer"`
}

type cLoadBalancer struct {
	Servers []cServer `yaml:"servers"`
}

type cServer struct {
	URL string `yaml:"url"`
}

// 172.17.0.1 is docker0's gateway, i.e. metal as seen from the Traefik
// container; 127.0.0.1 there would be Traefik's own loopback and 502 every
// delivery. Only POST /github is routed, so /healthz is never exposed.
func TestHooksRouteIsExactlyTheDispatcherRoute(t *testing.T) {
	got := decodeStrict[cHooksFile](t, filepath.Join(repoRoot(t), "traefik-hooks.yml"))
	want := cHooksFile{HTTP: cHTTP{
		Routers: map[string]cRouter{
			"bcx-hooks": {
				Rule:        "Host(`hooks.example.org`) && Path(`/github`) && Method(`POST`)",
				EntryPoints: []string{"websecure"},
				Service:     "bcx-hooks",
				TLS:         cTLS{CertResolver: "letsencrypt"},
			},
		},
		Services: map[string]cService{
			"bcx-hooks": {LoadBalancer: cLoadBalancer{Servers: []cServer{{URL: "http://172.17.0.1:9100"}}}},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("traefik-hooks.yml decoded to\n%+v\nwant\n%+v", got, want)
	}
}

// The per-tenant 9010/9011 routes are replaced, not extended: a leftover file
// would keep publishing the old webhook hosts next to the new one.
func TestOldPerTenantRoutesAreGone(t *testing.T) {
	_, err := os.Stat(filepath.Join(repoRoot(t), "traefik-runner-webhook.yml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("traefik-runner-webhook.yml must be removed, stat err = %v", err)
	}
}
