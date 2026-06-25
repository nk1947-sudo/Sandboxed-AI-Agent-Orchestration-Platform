//go:build linux

package orchestrator

import (
	"fmt"
	"os/exec"
	"strconv"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
)

// Egress networking is opt-in and default-off (see Config.EgressEnabled). When a
// launch requests it, each VM gets a dedicated TAP on the sandbox bridge
// (10.200.0.0/24) created by scripts/vm-tap.sh, plus a kernel ip= boot arg that
// configures the guest NIC. The host nftables ruleset (deploy/firewall) and the
// Squid forward proxy (deploy/squid) enforce default-deny egress with a dstdomain
// allowlist and drop IMDS. The guest reaches allowlisted APIs only via the proxy
// (set http_proxy/https_proxy=http://10.200.0.1:3128 in the guest).

const (
	egressSubnetPrefix = "10.200.0" // matches scripts/vm-tap.sh and deploy/firewall
	egressTapIndexMin  = 2          // index 1 is the host bridge / Squid gateway
	egressTapIndexMax  = 254
)

func (s *Supervisor) egressGatewayIP() string {
	if s.cfg.EgressGatewayIP != "" {
		return s.cfg.EgressGatewayIP
	}
	return egressSubnetPrefix + ".1"
}

func (s *Supervisor) egressNetmask() string {
	if s.cfg.EgressNetmask != "" {
		return s.cfg.EgressNetmask
	}
	return "255.255.255.0"
}

func (s *Supervisor) egressTapScript() string {
	if s.cfg.EgressTapScript != "" {
		return s.cfg.EgressTapScript
	}
	return "scripts/vm-tap.sh"
}

// tapNameFor derives the TAP device name for a sandbox id. Linux interface names
// are capped at 15 chars, so the id is truncated to match scripts/vm-tap.sh.
func tapNameFor(id string) string {
	if len(id) > 12 {
		id = id[:12]
	}
	return "vmtap-" + id
}

// setupEgress allocates a sandbox-subnet index, creates the per-VM TAP via the
// host script, and returns the Firecracker network interface plus the kernel ip=
// boot arg. On any failure it frees the index so nothing leaks.
func (s *Supervisor) setupEgress(id string) (tapName string, idx uint32, iface firecracker.NetworkInterface, bootArg string, err error) {
	idx, err = s.taps.alloc()
	if err != nil {
		return "", 0, firecracker.NetworkInterface{}, "", fmt.Errorf("egress: allocate tap index: %w", err)
	}
	tapName = tapNameFor(id)

	cmd := exec.Command(s.egressTapScript(), "create", id, strconv.Itoa(int(idx)))
	if out, e := cmd.CombinedOutput(); e != nil {
		s.taps.free(idx)
		return "", 0, firecracker.NetworkInterface{}, "", fmt.Errorf("egress: vm-tap create %s: %w: %s", id, e, out)
	}

	guestIP := fmt.Sprintf("%s.%d", egressSubnetPrefix, idx)
	mac := fmt.Sprintf("52:54:00:00:00:%02x", idx)
	iface = firecracker.NetworkInterface{
		StaticConfiguration: &firecracker.StaticNetworkConfiguration{
			MacAddress:  mac,
			HostDevName: tapName,
		},
		// Never let the guest reach the in-VMM MMDS datastore (defense in depth on
		// top of the nftables IMDS drop).
		AllowMMDS: false,
	}
	// ip=<client>::<gateway>:<netmask>::<iface>:<autoconf> — configures eth0 in the
	// guest at boot; "off" disables in-guest autoconfiguration (no DHCP).
	bootArg = fmt.Sprintf("ip=%s::%s:%s::eth0:off", guestIP, s.egressGatewayIP(), s.egressNetmask())
	return tapName, idx, iface, bootArg, nil
}

// teardownEgress destroys a VM's TAP and frees its subnet index. Best-effort:
// failures are logged, not returned, so they never block VM teardown.
func (s *Supervisor) teardownEgress(id, tapName string, idx uint32) {
	if tapName == "" {
		return
	}
	cmd := exec.Command(s.egressTapScript(), "destroy", id)
	if out, e := cmd.CombinedOutput(); e != nil {
		s.log.Warn("egress: vm-tap destroy failed", "id", id, "err", e, "out", string(out))
	}
	if idx != 0 {
		s.taps.free(idx)
	}
}
