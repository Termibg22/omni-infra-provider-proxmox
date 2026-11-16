// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v4"
)

// Route represents a static IP route for Cloud-Init network configuration.
type Route struct {
	To     string `yaml:"to" json:"to"`
	Via    string `yaml:"via" json:"via"`
	Metric uint32 `yaml:"metric,omitempty" json:"metric,omitempty"`
	OnLink bool   `yaml:"on-link,omitempty" json:"on_link,omitempty"`
}

// cloudInitNetConfigV2 matches the NoCloud NetworkConfig v2 format parsed by Talos Linux.
type cloudInitNetConfigV2 struct {
	Ethernets map[string]cloudInitEthernetV2 `yaml:"ethernets"`
	Bonds     map[string]cloudInitBondV2     `yaml:"bonds,omitempty"`
	Version   int                            `yaml:"version"`
}

type cloudInitEthernetV2 struct {
	Match       *cloudInitMatchV2       `yaml:"match,omitempty"`
	DHCP4       *bool                   `yaml:"dhcp4,omitempty"`
	DHCP6       *bool                   `yaml:"dhcp6,omitempty"`
	Nameservers *cloudInitNameserversV2 `yaml:"nameservers,omitempty"`
	Gateway4    string                  `yaml:"gateway4,omitempty"`
	Gateway6    string                  `yaml:"gateway6,omitempty"`
	Addresses   []string                `yaml:"addresses,omitempty"`
	Routes      []Route                 `yaml:"routes,omitempty"`
	MTU         int                     `yaml:"mtu,omitempty"`
}

type cloudInitBondV2 struct {
	DHCP4       *bool                   `yaml:"dhcp4,omitempty"`
	DHCP6       *bool                   `yaml:"dhcp6,omitempty"`
	Nameservers *cloudInitNameserversV2 `yaml:"nameservers,omitempty"`
	Parameters  *cloudInitBondParamsV2  `yaml:"parameters,omitempty"`
	Gateway4    string                  `yaml:"gateway4,omitempty"`
	Gateway6    string                  `yaml:"gateway6,omitempty"`
	Interfaces  []string                `yaml:"interfaces"`
	Addresses   []string                `yaml:"addresses,omitempty"`
	Routes      []Route                 `yaml:"routes,omitempty"`
	MTU         int                     `yaml:"mtu,omitempty"`
}

type cloudInitBondParamsV2 struct {
	Mode               string `yaml:"mode,omitempty"`
	LACPRate           string `yaml:"lacp-rate,omitempty"`
	TransmitHashPolicy string `yaml:"transmit-hash-policy,omitempty"`
	MIIMon             uint32 `yaml:"mii-monitor-interval,omitempty"`
}

type cloudInitMatchV2 struct {
	MACAddress string `yaml:"macaddress,omitempty"`
	Name       string `yaml:"name,omitempty"`
}

type cloudInitNameserversV2 struct {
	Addresses []string `yaml:"addresses,omitempty"`
	Search    []string `yaml:"search,omitempty"`
}

// ipPoolAllocator tracks in-memory IP allocations per pool and request ID so that
// multiple VMs provisioned from the same MachineClass with an `ip_pool` receive unique IPs.
type ipPoolAllocator struct {
	allocations map[string]map[string]netip.Addr // poolKey -> requestID -> addr
	mu          sync.Mutex
}

func newIPPoolAllocator() *ipPoolAllocator {
	return &ipPoolAllocator{
		allocations: make(map[string]map[string]netip.Addr),
	}
}

// Allocate picks or returns an existing IP in CIDR notation (e.g. "192.168.1.100/24")
// from poolSpec for requestID.
// Supported poolSpec formats:
//   - Range with prefix: "192.168.1.100-192.168.1.150/24"
//   - Subnet CIDR:       "192.168.1.0/24" (allocates usable host IPs)
//   - Single IP CIDR:    "192.168.1.50/24"
func (a *ipPoolAllocator) Allocate(poolSpec, requestID string) (string, error) {
	poolSpec = strings.TrimSpace(poolSpec)
	if poolSpec == "" {
		return "", nil
	}

	startAddr, endAddr, prefixBits, err := parseIPPool(poolSpec)
	if err != nil {
		return "", err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	byReq, ok := a.allocations[poolSpec]
	if !ok {
		byReq = make(map[string]netip.Addr)
		a.allocations[poolSpec] = byReq
	}

	if existing, found := byReq[requestID]; found {
		return fmt.Sprintf("%s/%d", existing.String(), prefixBits), nil
	}

	used := make(map[netip.Addr]struct{}, len(byReq))

	for _, requests := range a.allocations {
		for _, addr := range requests {
			used[addr] = struct{}{}
		}
	}

	// Build candidate list in range [startAddr, endAddr]
	var candidates []netip.Addr

	for cur := startAddr; cur.Compare(endAddr) <= 0; cur = cur.Next() {
		candidates = append(candidates, cur)

		if len(candidates) >= 65536 || cur == endAddr {
			break
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("ip_pool %q contains no valid IP addresses", poolSpec)
	}

	// Deterministic starting offset from requestID hash so restarts prefer the same IP
	h := fnv.New32a()
	_, _ = h.Write([]byte(requestID))
	startIdx := int(h.Sum32()) % len(candidates)

	for i := range candidates {
		cand := candidates[(startIdx+i)%len(candidates)]
		if _, taken := used[cand]; !taken {
			byReq[requestID] = cand

			return fmt.Sprintf("%s/%d", cand.String(), prefixBits), nil
		}
	}

	return "", fmt.Errorf("ip_pool %q is exhausted (%d addresses in use)", poolSpec, len(candidates))
}

func (a *ipPoolAllocator) Release(requestID string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, byReq := range a.allocations {
		for owner := range byReq {
			if owner == requestID || strings.HasPrefix(owner, requestID+"/") {
				delete(byReq, owner)
			}
		}
	}
}

func parseIPPool(spec string) (start, end netip.Addr, prefixBits int, err error) {
	// Format 1: "192.168.1.100-192.168.1.150/24"
	if strings.Contains(spec, "-") {
		return parseIPRangePool(spec)
	}

	// Format 2: "192.168.1.0/24" or single IP "192.168.1.50/24"
	prefix, parseErr := netip.ParsePrefix(spec)
	if parseErr != nil {
		return netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("invalid ip_pool CIDR %q: %w", spec, parseErr)
	}

	masked := prefix.Masked()
	// If the user gave a specific host IP (e.g. "192.168.1.50/24") or a single-host prefix (/32 or /128), return just that IP
	if prefix.Addr() != masked.Addr() || prefix.Bits() == prefix.Addr().BitLen() {
		return prefix.Addr(), prefix.Addr(), prefix.Bits(), nil
	}

	// RFC 3021 /31 IPv4 point-to-point subnet has two usable addresses
	if prefix.Addr().Is4() && prefix.Bits() == 31 {
		return masked.Addr(), masked.Addr().Next(), 31, nil
	}

	// Otherwise, compute usable host range in the subnet (skip network and broadcast on IPv4),
	// bounded to at most 4096 candidates so large subnets (e.g. /16 or IPv6 /64) never loop unboundedly.
	const maxSubnetScan = 4096

	first := masked.Addr().Next()
	last := first

	for count := 0; count < maxSubnetScan && masked.Contains(last.Next()); count++ {
		next := last.Next()
		// For IPv4, stop before the broadcast address (where next.Next() leaves the subnet)
		if prefix.Addr().Is4() && !masked.Contains(next.Next()) {
			break
		}

		last = next
	}

	return first, last, prefix.Bits(), nil
}

func parseIPRangePool(spec string) (start, end netip.Addr, prefixBits int, err error) {
	rangePart, prefixPart, hasSlash := strings.Cut(spec, "/")
	if !hasSlash {
		return netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("ip_pool %q must include a CIDR prefix length (e.g. 192.168.1.100-192.168.1.150/24)", spec)
	}

	startStr, endStr, _ := strings.Cut(rangePart, "-")

	startAddr, parseErr := netip.ParseAddr(strings.TrimSpace(startStr))
	if parseErr != nil {
		return netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("invalid start IP in ip_pool %q: %w", spec, parseErr)
	}

	endAddr, parseErr := netip.ParseAddr(strings.TrimSpace(endStr))
	if parseErr != nil {
		return netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("invalid end IP in ip_pool %q: %w", spec, parseErr)
	}

	p, parseErr := netip.ParsePrefix(fmt.Sprintf("%s/%s", startAddr.String(), strings.TrimSpace(prefixPart)))
	if parseErr != nil {
		return netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("invalid prefix length in ip_pool %q: %w", spec, parseErr)
	}

	if startAddr.BitLen() != endAddr.BitLen() || !p.Masked().Contains(endAddr) {
		return netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("ip_pool range %q must stay within one subnet and address family", spec)
	}

	if startAddr.Compare(endAddr) > 0 {
		return netip.Addr{}, netip.Addr{}, 0, fmt.Errorf("start IP %s is greater than end IP %s in ip_pool %q", startAddr, endAddr, spec)
	}

	return startAddr, endAddr, p.Bits(), nil
}

// extractVMMACAddresses parses Proxmox net0, net1, ... config strings
// (e.g. "virtio=BC:24:11:2E:15:A0,bridge=vmbr0,firewall=1") and returns the MAC per NIC index.
func extractVMMACAddresses(nets map[string]string, count int) map[int]string {
	result := make(map[int]string, count)

	for i := range count {
		key := fmt.Sprintf("net%d", i)

		val, ok := nets[key]
		if !ok || val == "" {
			continue
		}

		for part := range strings.SplitSeq(val, ",") {
			k, v, hasEq := strings.Cut(strings.TrimSpace(part), "=")
			if !hasEq {
				continue
			}

			switch strings.ToLower(k) {
			case "virtio", "e1000", "e1000e", "vmxnet3", "rtl8139", "macaddr":
				if strings.Contains(v, ":") {
					result[i] = strings.ToLower(v)
				}
			}
		}
	}

	return result
}

// buildCloudInitNetworkConfig generates the Cloud-Init network-config document.
// When all NICs use standard default DHCPv4 with no static addresses, bonds, routes, or custom DNS,
// it returns "version: 1" for full backward compatibility.
// When any NIC, Bond, or global DNS settings are customized, it builds a NoCloud Version 2 document.
func buildCloudInitNetworkConfig(data Data, nics []NIC, macByIndex map[int]string, allocatedIPByIndex map[int]string) (string, error) {
	if !hasCustomNetworkConfig(data, nics, allocatedIPByIndex) {
		return "version: 1", nil
	}

	cfg := cloudInitNetConfigV2{
		Version:   2,
		Ethernets: make(map[string]cloudInitEthernetV2, len(nics)),
	}

	enslavedIfaces := collectBondMemberInterfaces(data.Bonds)

	for i, nic := range nics {
		ifaceName := fmt.Sprintf("eth%d", i)

		var mac string
		if macByIndex != nil {
			mac = macByIndex[i]
		}

		_, enslaved := enslavedIfaces[ifaceName]

		eth, err := buildCloudInitEthernet(i, ifaceName, nic, data, mac, allocatedIPByIndex[i], enslaved)
		if err != nil {
			return "", err
		}

		cfg.Ethernets[ifaceName] = eth
	}

	if len(data.Bonds) > 0 {
		bonds, err := buildCloudInitBonds(data.Bonds, data)
		if err != nil {
			return "", err
		}

		cfg.Bonds = bonds
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal cloud-init network config: %w", err)
	}

	return string(out), nil
}

func collectBondMemberInterfaces(bonds []NetBond) map[string]struct{} {
	enslaved := map[string]struct{}{}

	for _, b := range bonds {
		for _, iface := range b.Interfaces {
			enslaved[normalizeBondInterfaceName(iface)] = struct{}{}
		}
	}

	return enslaved
}

func normalizeBondInterfaceName(raw string) string {
	s := strings.TrimSpace(raw)
	if idx, ok := strings.CutPrefix(s, "net"); ok && idx != "" {
		return "eth" + idx
	}

	return s
}

func buildCloudInitBonds(bonds []NetBond, data Data) (map[string]cloudInitBondV2, error) {
	result := make(map[string]cloudInitBondV2, len(bonds))

	for i, b := range bonds {
		name := cmp.Or(strings.TrimSpace(b.Name), fmt.Sprintf("bond%d", i))

		bondCfg, err := buildSingleCloudInitBond(name, b, data)
		if err != nil {
			return nil, err
		}

		result[name] = bondCfg
	}

	return result, nil
}

func buildSingleCloudInitBond(name string, b NetBond, data Data) (cloudInitBondV2, error) {
	if len(b.Interfaces) == 0 {
		return cloudInitBondV2{}, fmt.Errorf("bond %q must specify at least one member interface (e.g. eth0, eth1)", name)
	}

	normIfaces := make([]string, 0, len(b.Interfaces))
	for _, iface := range b.Interfaces {
		if norm := normalizeBondInterfaceName(iface); norm != "" {
			normIfaces = append(normIfaces, norm)
		}
	}

	validAddrs, err := validateCIDRAddresses(name, b.Addresses)
	if err != nil {
		return cloudInitBondV2{}, err
	}

	dhcp4 := len(validAddrs) == 0
	if b.DHCP4 != nil {
		dhcp4 = *b.DHCP4
	}

	mode := cmp.Or(strings.TrimSpace(b.Mode), "802.3ad")
	params := &cloudInitBondParamsV2{
		Mode:               mode,
		MIIMon:             cmp.Or(b.MIIMonitorInterval, uint32(100)),
		TransmitHashPolicy: b.TransmitHashPolicy,
	}

	if mode == "802.3ad" {
		params.LACPRate = cmp.Or(b.LACPRate, "fast")
		params.TransmitHashPolicy = cmp.Or(b.TransmitHashPolicy, "layer2+3")
	}

	bondCfg := cloudInitBondV2{
		Interfaces: normIfaces,
		Parameters: params,
		DHCP4:      &dhcp4,
		Addresses:  validAddrs,
		Gateway4:   strings.TrimSpace(b.Gateway4),
		Gateway6:   strings.TrimSpace(b.Gateway6),
		Routes:     b.Routes,
		MTU:        b.MTU,
	}

	if b.DHCP6 != nil && *b.DHCP6 {
		dhcp6 := true
		bondCfg.DHCP6 = &dhcp6
	}

	bondCfg.Nameservers = resolveBondNameservers(b, data)

	return bondCfg, nil
}

func validateCIDRAddresses(owner string, rawAddrs []string) ([]string, error) {
	var valid []string

	for _, addr := range rawAddrs {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}

		if _, err := netip.ParsePrefix(addr); err != nil {
			return nil, fmt.Errorf("bond %q: invalid CIDR address %q: %w", owner, addr, err)
		}

		if !slices.Contains(valid, addr) {
			valid = append(valid, addr)
		}
	}

	return valid, nil
}

func resolveBondNameservers(b NetBond, data Data) *cloudInitNameserversV2 {
	addrs := b.Nameservers
	if len(addrs) == 0 {
		addrs = data.DNSServers
	}

	search := b.SearchDomains
	if len(search) == 0 {
		search = data.DNSSearchDomains
	}

	if len(addrs) == 0 && len(search) == 0 {
		return nil
	}

	return &cloudInitNameserversV2{
		Addresses: addrs,
		Search:    search,
	}
}

func hasCustomNetworkConfig(data Data, nics []NIC, allocatedIPByIndex map[int]string) bool {
	if len(data.DNSServers) > 0 || len(data.DNSSearchDomains) > 0 || len(data.Bonds) > 0 {
		return true
	}

	for i, nic := range nics {
		if len(nic.Addresses) > 0 || allocatedIPByIndex[i] != "" || nic.IPPool != "" ||
			nic.Gateway4 != "" || nic.Gateway6 != "" ||
			len(nic.Nameservers) > 0 || len(nic.SearchDomains) > 0 || len(nic.Routes) > 0 ||
			nic.MTU > 1 || (nic.DHCP4 != nil && !*nic.DHCP4) || (nic.DHCP6 != nil && *nic.DHCP6) {
			return true
		}
	}

	return false
}

func buildCloudInitEthernet(
	index int,
	ifaceName string,
	nic NIC,
	data Data,
	assignedMAC string,
	allocatedIP string,
	enslavedInBond bool,
) (cloudInitEthernetV2, error) {
	var addresses []string
	if allocatedIP != "" {
		addresses = append(addresses, allocatedIP)
	}

	for _, addr := range nic.Addresses {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}

		if _, err := netip.ParsePrefix(addr); err != nil {
			return cloudInitEthernetV2{}, fmt.Errorf("NIC %d (%s): invalid CIDR address %q (expected format like 192.168.1.10/24): %w", index, ifaceName, addr, err)
		}

		if !slices.Contains(addresses, addr) {
			addresses = append(addresses, addr)
		}
	}

	dhcp4 := !enslavedInBond && len(addresses) == 0
	if nic.DHCP4 != nil && !enslavedInBond {
		dhcp4 = *nic.DHCP4
	}

	eth := cloudInitEthernetV2{
		DHCP4:     &dhcp4,
		Addresses: addresses,
		Gateway4:  strings.TrimSpace(nic.Gateway4),
		Gateway6:  strings.TrimSpace(nic.Gateway6),
	}

	if nic.DHCP6 != nil && *nic.DHCP6 && !enslavedInBond {
		dhcp6 := true
		eth.DHCP6 = &dhcp6
	}

	if nic.MTU > 1 {
		eth.MTU = nic.MTU
	}

	mac := cmp.Or(strings.ToLower(strings.TrimSpace(nic.MACAddress)), assignedMAC)
	if mac != "" {
		eth.Match = &cloudInitMatchV2{MACAddress: mac}
	}

	if enslavedInBond {
		eth.Addresses = nil
		eth.Gateway4 = ""
		eth.Gateway6 = ""

		return eth, nil
	}

	if !enslavedInBond {
		if ns := resolveNICNameservers(index, nic, data); ns != nil {
			eth.Nameservers = ns
		}
	}

	for _, r := range nic.Routes {
		if r.To != "" && r.Via != "" {
			eth.Routes = append(eth.Routes, r)
		}
	}

	return eth, nil
}

func resolveNICNameservers(index int, nic NIC, data Data) *cloudInitNameserversV2 {
	nsAddrs := slices.Clone(nic.Nameservers)
	if len(nsAddrs) == 0 && index == 0 && len(data.DNSServers) > 0 {
		nsAddrs = slices.Clone(data.DNSServers)
	}

	nsSearch := slices.Clone(nic.SearchDomains)
	if len(nsSearch) == 0 && index == 0 && len(data.DNSSearchDomains) > 0 {
		nsSearch = slices.Clone(data.DNSSearchDomains)
	}

	if len(nsAddrs) == 0 && len(nsSearch) == 0 {
		return nil
	}

	return &cloudInitNameserversV2{
		Addresses: nsAddrs,
		Search:    nsSearch,
	}
}

// Reserve restores a committed address and refuses conflicting persisted state.
func (a *ipPoolAllocator) Reserve(pool, owner, address string) error {
	prefix, err := netip.ParsePrefix(address)
	if err != nil {
		return fmt.Errorf("invalid persisted IP address %q: %w", address, err)
	}

	start, end, bits, err := parseIPPool(strings.TrimSpace(pool))
	if err != nil {
		return err
	}

	addr := prefix.Addr()
	if prefix.Bits() != bits || addr.Compare(start) < 0 || addr.Compare(end) > 0 {
		return fmt.Errorf("persisted IP %q is outside pool %q", address, pool)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, requests := range a.allocations {
		for other, used := range requests {
			if other != owner && used == addr {
				return fmt.Errorf("IP %s is already reserved by %q", addr, other)
			}
		}
	}

	pool = strings.TrimSpace(pool)
	if a.allocations[pool] == nil {
		a.allocations[pool] = map[string]netip.Addr{}
	}

	a.allocations[pool][owner] = addr

	return nil
}

// Drop reservations for resources that have disappeared from durable state.
// This also handles a cleanup racing with a stale state-list response.
func (a *ipPoolAllocator) pruneOwners(known map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, requests := range a.allocations {
		for owner := range requests {
			boundary := strings.LastIndex(owner, "/")
			if boundary >= 0 && !known[owner[:boundary]] {
				delete(requests, owner)
			}
		}
	}
}
