// Package deploytest tests the deploy artifacts that are not Go: systemd units,
// install.sh, build.sh, broker-firewall.sh, traefik-hooks.yml and tiers.yml. It has
// no production code; it exists so `go test` stays the single gate for the module.
package deploytest
