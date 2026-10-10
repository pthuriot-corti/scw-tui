package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/scaleway/scaleway-sdk-go/api/account/v3"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/api/lb/v1"
	"github.com/scaleway/scaleway-sdk-go/api/vpcgw/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

// Row is a generic table row: display columns + an ID for the resource.
type Row struct {
	ID   string
	Cols []string
}

// ResourceKind enumerates the resource types the TUI can display.
type ResourceKind int

const (
	KindInstances ResourceKind = iota
	KindImages
	KindBuckets
	KindLoadBalancers
	KindGateways
	KindSKUs
)

func (k ResourceKind) String() string {
	switch k {
	case KindInstances:
		return "instances"
	case KindImages:
		return "images"
	case KindBuckets:
		return "buckets"
	case KindLoadBalancers:
		return "loadbalancers"
	case KindGateways:
		return "gateways"
	case KindSKUs:
		return "skus"
	default:
		return "unknown"
	}
}

// Header returns the column titles for a given resource kind.
func (k ResourceKind) Header() []string {
	switch k {
	case KindInstances:
		return []string{"Name", "ID", "Type", "State", "Zone", "Public IP", "Private IP", "Created"}
	case KindImages:
		return []string{"Name", "ID", "Arch", "State", "Public", "Zone", "Created"}
	case KindBuckets:
		return []string{"Name", "Region", "Created"}
	case KindLoadBalancers:
		return []string{"Name", "ID", "Status", "Type", "Zone", "IP", "Frontends", "Backends", "Created"}
	case KindGateways:
		return []string{"Name", "ID", "Status", "Type", "Zone", "IP", "Created"}
	case KindSKUs:
		return []string{"Name", "Arch", "CPUs", "RAM", "GPU", "Hourly Price", "Zone", "SKU"}
	default:
		return nil
	}
}

// fetchInstances lists servers across all zones, optionally filtered by project.
func fetchInstances(c *Clients, projectID *string) ([]Row, error) {
	var rows []Row
	resp, err := c.instance.ListServers(&instance.ListServersRequest{Project: projectID}, scw.WithAllPages(), scw.WithZones(scw.AllZones...))
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	for _, s := range resp.Servers {
		pubIP := ""
		if len(s.PublicIPs) > 0 && s.PublicIPs[0].Address != nil {
			pubIP = s.PublicIPs[0].Address.String()
		} else if s.PublicIP != nil && s.PublicIP.Address != nil {
			pubIP = s.PublicIP.Address.String()
		}
		privIP := ""
		if s.PrivateIP != nil {
			privIP = *s.PrivateIP
		}
		rows = append(rows, Row{
			ID: s.ID,
			Cols: []string{
				s.Name,
				s.ID,
				s.CommercialType,
				string(s.State),
				s.Zone.String(),
				pubIP,
				privIP,
				fmtTime(s.CreationDate),
			},
		})
	}
	return rows, nil
}

// fetchImages lists images across all zones, optionally filtered by project.
func fetchImages(c *Clients, projectID *string) ([]Row, error) {
	var rows []Row
	resp, err := c.instance.ListImages(&instance.ListImagesRequest{Project: projectID}, scw.WithAllPages(), scw.WithZones(scw.AllZones...))
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	for _, img := range resp.Images {
		rows = append(rows, Row{
			ID: img.ID,
			Cols: []string{
				img.Name,
				img.ID,
				string(img.Arch),
				string(img.State),
				fmt.Sprintf("%v", img.Public),
				img.Zone.String(),
				fmtTime(img.CreationDate),
			},
		})
	}
	return rows, nil
}

// fetchLoadBalancers lists LBs per-region, optionally filtered by project, skipping unavailable regions.
func fetchLoadBalancers(c *Clients, projectID *string) ([]Row, error) {
	var rows []Row
	for _, region := range scw.AllRegions {
		resp, err := c.lb.ListLBs(&lb.ListLBsRequest{Region: region, ProjectID: projectID}, scw.WithAllPages())
		if err != nil {
			if isNotImplemented(err) {
				continue
			}
			return nil, fmt.Errorf("list lbs in %s: %w", region, err)
		}
		for _, lb := range resp.LBs {
			ips := make([]string, 0, len(lb.IP))
			for _, ip := range lb.IP {
				ips = append(ips, ip.IPAddress)
			}
			rows = append(rows, Row{
				ID: lb.ID,
				Cols: []string{
					lb.Name,
					lb.ID,
					string(lb.Status),
					lb.Type,
					lb.Zone.String(),
					strings.Join(ips, ", "),
					fmt.Sprintf("%d", lb.FrontendCount),
					fmt.Sprintf("%d", lb.BackendCount),
					fmtTime(lb.CreatedAt),
				},
			})
		}
	}
	return rows, nil
}

// fetchGateways lists public gateways per-zone, optionally filtered by project, skipping unavailable zones.
func fetchGateways(c *Clients, projectID *string) ([]Row, error) {
	var rows []Row
	for _, zone := range scw.AllZones {
		resp, err := c.vpcgw.ListGateways(&vpcgw.ListGatewaysRequest{Zone: zone, ProjectID: projectID}, scw.WithAllPages())
		if err != nil {
			if isNotImplemented(err) {
				continue
			}
			return nil, fmt.Errorf("list gateways in %s: %w", zone, err)
		}
		for _, gw := range resp.Gateways {
			gwType := ""
			if gw.Type != nil {
				gwType = gw.Type.Name
			}
			ip := ""
			if gw.IP != nil && gw.IP.Address != nil {
				ip = gw.IP.Address.String()
			}
			rows = append(rows, Row{
				ID: gw.ID,
				Cols: []string{
					gw.Name,
					gw.ID,
					string(gw.Status),
					gwType,
					gw.Zone.String(),
					ip,
					fmtTime(gw.CreatedAt),
				},
			})
		}
	}
	return rows, nil
}

// fetchBuckets lists object storage buckets via S3 API. S3 does not support project filtering.
func fetchBuckets(c *Clients, _ *string) ([]Row, error) {
	result, err := c.s3.ListBuckets(ctx, &awss3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}
	var rows []Row
	for _, b := range result.Buckets {
		rows = append(rows, Row{
			ID: *b.Name,
			Cols: []string{
				*b.Name,
				string(c.region),
				fmtTime(b.CreationDate),
			},
		})
	}
	return rows, nil
}

// fetchSKUs lists instance server types with per-zone availability in the given region.
func fetchSKUs(c *Clients, _ scw.Region, zones []scw.Zone) ([]Row, error) {
	type zoneResult struct {
		types        map[string]*instance.ServerType
		availability map[string]instance.ServerTypesAvailability
		err          error
	}

	results := make([]zoneResult, len(zones))
	var wg sync.WaitGroup
	wg.Add(len(zones))
	for i, zone := range zones {
		go func(i int, zone scw.Zone) {
			defer wg.Done()
			typesResp, err := c.instance.ListServersTypes(&instance.ListServersTypesRequest{Zone: zone}, scw.WithAllPages())
			if err != nil {
				results[i] = zoneResult{err: err}
				return
			}
			availResp, err := c.instance.GetServerTypesAvailability(&instance.GetServerTypesAvailabilityRequest{Zone: zone}, scw.WithAllPages())
			if err != nil {
				// availability is best-effort; use types only
				results[i] = zoneResult{types: typesResp.Servers}
				return
			}
			avail := make(map[string]instance.ServerTypesAvailability, len(availResp.Servers))
			for name, a := range availResp.Servers {
				avail[name] = a.Availability
			}
			results[i] = zoneResult{types: typesResp.Servers, availability: avail}
		}(i, zone)
	}
	wg.Wait()

	// Merge: one row per server type name, with availability per zone.
	merged := make(map[string]*instance.ServerType)
	availability := make(map[string]map[int]instance.ServerTypesAvailability)

	for i, zr := range results {
		if zr.err != nil {
			continue
		}
		for name, st := range zr.types {
			if _, exists := merged[name]; !exists {
				merged[name] = st
				availability[name] = make(map[int]instance.ServerTypesAvailability)
			}
			if zr.availability != nil {
				availability[name][i] = zr.availability[name]
			}
		}
	}

	var rows []Row
	for name, st := range merged {
		gpu := "-"
		if st.Gpu != nil && *st.Gpu > 0 {
			gpu = fmt.Sprintf("%d", *st.Gpu)
		}
		cols := []string{
			name,
			string(st.Arch),
			fmt.Sprintf("%d", st.Ncpus),
			fmtRAM(st.RAM),
			gpu,
		}
		for i := range zones {
			cols = append(cols, fmtAvailability(availability[name][i]))
		}
		rows = append(rows, Row{ID: name, Cols: cols})
	}
	return rows, nil
}

// fmtAvailability converts a ServerTypesAvailability to a display string.
func fmtAvailability(a instance.ServerTypesAvailability) string {
	switch a {
	case instance.ServerTypesAvailabilityAvailable:
		return "✓"
	case instance.ServerTypesAvailabilityScarce:
		return "low"
	case instance.ServerTypesAvailabilityShortage:
		return "out"
	default:
		return "—"
	}
}

// fetchInstanceDetail fetches full details for a specific instance.
func fetchInstanceDetail(c *Clients, serverID, zoneStr string) ([]string, error) {
	zone := scw.Zone(zoneStr)
	resp, err := c.instance.GetServer(&instance.GetServerRequest{Zone: zone, ServerID: serverID})
	if err != nil {
		return nil, fmt.Errorf("get server: %w", err)
	}
	s := resp.Server

	pubIPs := make([]string, 0, len(s.PublicIPs))
	for _, ip := range s.PublicIPs {
		if ip.Address != nil {
			pubIPs = append(pubIPs, fmt.Sprintf("%s (%s, %s)", ip.Address.String(), ip.Family, ipState(ip)))
		}
	}
	if s.PublicIP != nil && s.PublicIP.Address != nil {
		pubIPs = append(pubIPs, s.PublicIP.Address.String()+" (deprecated)")
	}

	volumes := make([]string, 0, len(s.Volumes))
	for _, v := range s.Volumes {
		name := ""
		if v.Name != nil {
			name = *v.Name
		}
		size := ""
		if v.Size != nil {
			size = fmt.Sprintf("%.1f GiB", float64(*v.Size)/float64(1e9))
		}
		volumes = append(volumes, fmt.Sprintf("%s (%s, %s, %s)", name, v.ID, v.VolumeType, size))
	}

	actions := make([]string, 0, len(s.AllowedActions))
	for _, a := range s.AllowedActions {
		actions = append(actions, string(a))
	}

	tags := strings.Join(s.Tags, ", ")

	secGroup := "none"
	if s.SecurityGroup != nil {
		secGroup = fmt.Sprintf("%s (%s)", s.SecurityGroup.Name, s.SecurityGroup.ID)
	}

	image := "none"
	if s.Image != nil {
		image = fmt.Sprintf("%s (%s, %s)", s.Image.Name, s.Image.ID, s.Image.Arch)
	}

	privNics := make([]string, 0, len(s.PrivateNics))
	for _, nic := range s.PrivateNics {
		privNics = append(privNics, fmt.Sprintf("%s (PN: %s, MAC: %s, %s)", nic.ID, nic.PrivateNetworkID, nic.MacAddress, nic.State))
	}

	filesystems := make([]string, 0, len(s.Filesystems))
	for _, fs := range s.Filesystems {
		filesystems = append(filesystems, fmt.Sprintf("%s (%s)", fs.FilesystemID, fs.State))
	}

	routedIP := "false"
	if s.RoutedIPEnabled != nil && *s.RoutedIPEnabled {
		routedIP = "true"
	}

	ipv6 := "none"
	if s.IPv6 != nil && s.IPv6.Address != nil {
		ipv6 = s.IPv6.Address.String()
	}

	dns := "none"
	if s.DNS != nil {
		dns = *s.DNS
	}

	placementGroup := "none"
	if s.PlacementGroup != nil {
		placementGroup = fmt.Sprintf("%s (%s)", s.PlacementGroup.Name, s.PlacementGroup.ID)
	}

	maintenances := make([]string, 0, len(s.Maintenances))
	for _, m := range s.Maintenances {
		maintenances = append(maintenances, fmt.Sprintf("%s (%s)", m.Reason, fmtTime(m.StartDate)))
	}

	detail := []string{
		"Name: " + s.Name,
		"ID: " + s.ID,
		"State: " + string(s.State),
		"State Detail: " + s.StateDetail,
		"Commercial Type: " + s.CommercialType,
		"Arch: " + string(s.Arch),
		"Zone: " + s.Zone.String(),
		"Hostname: " + s.Hostname,
		"Boot Type: " + string(s.BootType),
		"Organization: " + s.Organization,
		"Project: " + s.Project,
		"Image: " + image,
		"Public IPs: " + strings.Join(pubIPs, "\n               "),
		"Private IP: " + ptrStr(s.PrivateIP),
		"IPv6: " + ipv6,
		"DNS: " + dns,
		"Dynamic IP Required: " + fmt.Sprintf("%v", s.DynamicIPRequired),
		"Routed IP Enabled: " + routedIP,
		"MAC Address: " + s.MacAddress,
		"Security Group: " + secGroup,
		"Placement Group: " + placementGroup,
		"Protected: " + fmt.Sprintf("%v", s.Protected),
		"End of Service: " + fmt.Sprintf("%v", s.EndOfService),
		"Tags: " + tags,
		"Allowed Actions: " + strings.Join(actions, ", "),
		"Volumes: " + strings.Join(volumes, "\n               "),
		"Private NICs: " + strings.Join(privNics, "\n               "),
		"Filesystems: " + strings.Join(filesystems, ", "),
		"Maintenances: " + strings.Join(maintenances, ", "),
		"Created: " + fmtTime(s.CreationDate),
		"Modified: " + fmtTime(s.ModificationDate),
	}
	return detail, nil
}

func ptrStr(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}

func ipState(ip *instance.ServerIP) string {
	return string(ip.State)
}

// SKU memory stored in bytes, format to human-readable.
func fmtRAM(bytes uint64) string {
	const gib = 1024 * 1024 * 1024
	const mib = 1024 * 1024
	if bytes >= gib {
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
	}
	return fmt.Sprintf("%.0f MiB", float64(bytes)/float64(mib))
}

// fetchSKUDetail fetches specs for a specific server type in a zone.
func fetchSKUDetail(c *Clients, typeName, zoneStr string) ([]string, error) {
	zone := scw.Zone(zoneStr)
	resp, err := c.instance.ListServersTypes(&instance.ListServersTypesRequest{Zone: zone}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("list server types: %w", err)
	}
	st, ok := resp.Servers[typeName]
	if !ok {
		return nil, fmt.Errorf("server type %s not found in %s", typeName, zone)
	}

	detail := []string{
		"Name: " + typeName,
		"Zone: " + zoneStr,
		"SKU: " + st.Sku,
		"Arch: " + string(st.Arch),
		"CPUs: " + fmt.Sprintf("%d", st.Ncpus),
		"RAM: " + fmtRAM(st.RAM),
		"Hourly Price: " + fmt.Sprintf("€%.4f", st.HourlyPrice),
	}
	if st.MonthlyPrice != nil {
		detail = append(detail, "Monthly Price: "+fmt.Sprintf("€%.2f", *st.MonthlyPrice))
	}
	if st.Gpu != nil && *st.Gpu > 0 {
		detail = append(detail, "GPUs: "+fmt.Sprintf("%d", *st.Gpu))
		if st.GpuInfo != nil {
			detail = append(detail, "GPU Model: "+st.GpuInfo.GpuManufacturer+" "+st.GpuInfo.GpuName)
			detail = append(detail, "GPU Memory: "+fmtRAM(uint64(st.GpuInfo.GpuMemory)))
		}
	}
	if st.Network != nil {
		if st.Network.SumInternalBandwidth != nil {
			detail = append(detail, "Internal Bandwidth: "+fmtBandwidth(*st.Network.SumInternalBandwidth))
		}
		if st.Network.SumInternetBandwidth != nil {
			detail = append(detail, "Internet Bandwidth: "+fmtBandwidth(*st.Network.SumInternetBandwidth))
		}
		detail = append(detail, "IPv6 Support: "+fmt.Sprintf("%v", st.Network.IPv6Support))
	}
	if st.Capabilities != nil {
		if st.Capabilities.BlockStorage != nil {
			detail = append(detail, "Block Storage: "+fmt.Sprintf("%v", *st.Capabilities.BlockStorage))
		}
		detail = append(detail, "Max File Systems: "+fmt.Sprintf("%d", st.Capabilities.MaxFileSystems))
	}
	if st.EndOfService {
		detail = append(detail, "End of Service: true")
	}
	return detail, nil
}

func fmtBandwidth(bps uint64) string {
	const gbps = 1_000_000_000
	const mbps = 1_000_000
	if bps >= gbps {
		return fmt.Sprintf("%.1f Gbps", float64(bps)/float64(gbps))
	}
	return fmt.Sprintf("%.0f Mbps", float64(bps)/float64(mbps))
}

// ProjectInfo holds a project's ID and name for the project selector.
type ProjectInfo struct {
	ID   string
	Name string
}

// fetchProjects lists all projects accessible to the current credentials.
func fetchProjects(c *Clients) ([]ProjectInfo, error) {
	resp, err := c.account.ListProjects(&account.ProjectAPIListProjectsRequest{}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	projects := make([]ProjectInfo, 0, len(resp.Projects))
	for _, p := range resp.Projects {
		projects = append(projects, ProjectInfo{ID: p.ID, Name: p.Name})
	}
	return projects, nil
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format("2006-01-02 15:04")
}

// isNotImplemented returns true for HTTP 501, raised when a service is not available in a region/zone.
func isNotImplemented(err error) bool {
	var respErr *scw.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == 501
	}
	return false
}
