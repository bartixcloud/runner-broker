package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bartixcloud/runner-broker/hcloud"
)

const (
	hcloudFirewallName = "bcx-ci-deny-inbound"
	// Hetzner rejects user_data at 32 KiB. Refuse it here, before any API call.
	maxHcloudUserData = 32 << 10
)

// hcloudLaunchTimeout bounds snapshot lookup, firewall ensure and the location
// loop together. It stays under the broker's 30 s drainTimeout (spec §10).
var hcloudLaunchTimeout = 25 * time.Second

// hcloudOps is the slice of the Hetzner client the backend uses. A test swaps a
// fake; production is *hcloud.Client.
type hcloudOps interface {
	ListServersContext(context.Context, string) ([]hcloud.Server, error)
	CreateServerContext(context.Context, hcloud.CreateRequest) (hcloud.Server, error)
	DeleteServer(int64) error
	ListSnapshotsContext(context.Context, string) ([]hcloud.Snapshot, error)
	EnsureFirewallContext(context.Context, string) (int64, error)
}

// hcloudAPI is the process's Hetzner client, installed once at startup. A var
// so tests can swap a fake and so the zero-value hcloudBackend stays useful,
// exactly like the incus backend's runningRunners.
var hcloudAPI hcloudOps

// hcloudBackend is the Hetzner host behind the backend seam. It never deletes a
// server it did not find through the tenant selector.
type hcloudBackend struct{}

func (hcloudBackend) ops() (hcloudOps, error) {
	if hcloudAPI == nil {
		return nil, errors.New("hcloud backend has no client: this tenant's hcloud token is not configured")
	}
	return hcloudAPI, nil
}

// hcloudSelector is the one listing scope: managed by us, this tenant, and
// optionally one tier. Source: docs.hetzner.cloud/reference/cloud — a label
// selector is key=value, comma-separated for AND.
func hcloudSelector(t string) string {
	selector := "bcx-managed=ci,bcx-tenant=" + tenant
	if t != "" {
		selector += ",bcx-tier=" + t
	}
	return selector
}

func (b hcloudBackend) Running(t string) ([]string, error) {
	api, err := b.ops()
	if err != nil {
		return nil, err
	}
	servers, err := api.ListServersContext(context.Background(), hcloudSelector(t))
	if err != nil {
		return nil, fmt.Errorf("cannot list hcloud servers for tier %q: %w", t, err)
	}
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Name)
	}
	return out, nil
}

func (b hcloudBackend) Admit(t string, cfg tier, reserved int) bool {
	current, err := b.Running(t)
	if err != nil {
		logf("ERROR", "try_reserve(%s): cannot determine current hcloud count, refusing reservation: %s", t, err)
		return false
	}
	if len(current)+reserved >= cfg.MaxRunners {
		return false
	}
	return true
}

func (b hcloudBackend) Launch(spec launchSpec) error {
	api, err := b.ops()
	if err != nil {
		return err
	}
	userData, err := buildHcloudUserData(spec.JIT)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hcloudLaunchTimeout)
	defer cancel()

	snapshots, err := api.ListSnapshotsContext(ctx, "bcx-image="+spec.Config.Image)
	if err != nil {
		return fmt.Errorf("cannot resolve hcloud snapshot for image %q: %w", spec.Config.Image, err)
	}
	var snapshot hcloud.Snapshot
	for _, s := range snapshots {
		if s.ID > 0 {
			snapshot = s
			break
		}
	}
	if snapshot.ID < 1 {
		return fmt.Errorf("no snapshot labelled bcx-image=%s: refusing to fall back to Incus or an unlabelled image", spec.Config.Image)
	}

	firewallID, err := api.EnsureFirewallContext(ctx, hcloudFirewallName)
	if err != nil {
		return fmt.Errorf("cannot ensure hcloud firewall %q: %w", hcloudFirewallName, err)
	}

	tierName, jobID := hcloudNameParts(spec)
	req := hcloud.CreateRequest{
		Name:       spec.Name,
		ServerType: spec.Config.ServerType,
		Image:      snapshot.ID,
		UserData:   userData,
		Labels: map[string]string{
			"bcx-managed": "ci",
			"bcx-tenant":  tenant,
			"bcx-tier":    tierName,
			"bcx-job":     strconv.Itoa(jobID),
		},
		Firewalls:        []hcloud.FirewallRef{{Firewall: firewallID}},
		PublicNet:        hcloud.PublicNet{EnableIPv4: true, EnableIPv6: false},
		StartAfterCreate: true,
	}

	retriedName := false
	for i := 0; i < len(spec.Config.Locations); {
		if ctx.Err() != nil {
			logf("WARNING", "hcloud create for tier %s ran out of time after %s — job waits for the reconciler",
				tierName, hcloudLaunchTimeout)
			return errLaunchAbandoned
		}
		req.Location = spec.Config.Locations[i]
		_, err := api.CreateServerContext(ctx, req)
		if err == nil {
			logf("INFO", "hcloud created %s in %s (tier=%s)", req.Name, req.Location, tierName)
			return nil
		}
		var apiErr *hcloud.APIError
		if !errors.As(err, &apiErr) {
			if ctx.Err() != nil {
				logf("WARNING", "hcloud create for tier %s timed out — job waits for the reconciler", tierName)
				return errLaunchAbandoned
			}
			return err
		}
		switch apiErr.Code {
		case "resource_unavailable", "placement_error":
			i++
			continue
		case "resource_limit_exceeded", "rate_limit_exceeded":
			logf("WARNING", "hcloud create for tier %s is limited (%s) — the job waits for the reconciler; "+
				"raise the project limit or retry after the rate window", tierName, apiErr.Code)
			return errLaunchAbandoned
		case "uniqueness_error":
			if retriedName {
				return err
			}
			newName, ok := hcloudRetryName(spec)
			if !ok {
				return err
			}
			if spec.Remint != nil {
				newJIT, remintErr := spec.Remint(newName)
				if remintErr != nil {
					return remintErr
				}
				spec.JIT = newJIT
				userData, remintErr = buildHcloudUserData(spec.JIT)
				if remintErr != nil {
					return remintErr
				}
				req.UserData = userData
			}
			req.Name = newName
			retriedName = true
			// A name collision creates nothing, so retrying the same location is safe.
			continue
		default:
			return err
		}
	}
	logf("WARNING", "hcloud create for tier %s found no capacity in %v — the job waits for the reconciler",
		tierName, spec.Config.Locations)
	return errLaunchAbandoned
}

func (b hcloudBackend) Delete(name string) error {
	api, err := b.ops()
	if err != nil {
		return err
	}
	servers, err := api.ListServersContext(context.Background(), hcloudSelector(""))
	if err != nil {
		return fmt.Errorf("cannot list hcloud servers to delete %s: %w", name, err)
	}
	for _, s := range servers {
		if s.Name != name {
			continue
		}
		if s.ID < 1 {
			return fmt.Errorf("hcloud server %s has no positive id", name)
		}
		return api.DeleteServer(s.ID)
	}
	return nil
}

// hcloudNameParts returns the tier and job id encoded in ci-<tier>-<jobID>-<hex6>.
// launchSpec carries them explicitly; the name parse is the fallback for a
// direct backend caller.
func hcloudNameParts(spec launchSpec) (string, int) {
	if spec.Tier != "" && spec.JobID > 0 {
		return spec.Tier, spec.JobID
	}
	fields := strings.Split(spec.Name, "-")
	if len(fields) < 4 || fields[0] != "ci" {
		return "", 0
	}
	id, err := strconv.Atoi(fields[len(fields)-2])
	if err != nil || id < 1 {
		return "", 0
	}
	return strings.Join(fields[1:len(fields)-2], "-"), id
}

// hcloudRetryName regenerates the six hex chars that Hetzner found taken. The
// runner name is part of the JIT config, so spawn also remints it through
// spec.Remint when a collision actually happens.
func hcloudRetryName(spec launchSpec) (string, bool) {
	tierName, jobID := hcloudNameParts(spec)
	if tierName == "" || jobID < 1 {
		return "", false
	}
	return fmt.Sprintf("ci-%s-%d-%s", tierName, jobID, randHex()), true
}

// buildHcloudUserData writes the single-use JIT config into the snapshot's
// runner directory and starts the baked one-job unit. The size check is the
// documented Hetzner 32 KiB user_data limit, enforced before any API call.
func buildHcloudUserData(jit string) (string, error) {
	encoded := base64.StdEncoding.EncodeToString([]byte(jit))
	userData := "#cloud-config\n" +
		"write_files:\n" +
		"  - path: /etc/actions-runner/jitconfig\n" +
		"    permissions: '0600'\n" +
		"    owner: runner:runner\n" +
		"    encoding: b64\n" +
		"    content: " + encoded + "\n" +
		"runcmd:\n" +
		"  - [systemctl, start, --no-block, bcx-ci-runner.service]\n"
	if len(userData) >= maxHcloudUserData {
		return "", fmt.Errorf("hcloud user_data is %d bytes, must be < 32 KiB", len(userData))
	}
	return userData, nil
}
