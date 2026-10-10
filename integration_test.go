package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/scaleway/scaleway-sdk-go/api/account/v3"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/api/lb/v1"
	"github.com/scaleway/scaleway-sdk-go/api/vpcgw/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

// mockServer routes Scaleway and S3 API requests to handler functions.
// Each handler is keyed by URL path prefix.
type mockServer struct {
	t        *testing.T
	server   *httptest.Server
	handlers map[string]func(r *http.Request) (any, error)
}

func newMockServer(t *testing.T) *mockServer {
	t.Helper()
	ms := &mockServer{
		t:        t,
		handlers: make(map[string]func(r *http.Request) (any, error)),
	}
	ms.server = httptest.NewServer(http.HandlerFunc(ms.handle))
	t.Cleanup(ms.server.Close)
	// SDK calls /metadata after list calls to set SRN fields.
	ms.on("/metadata", func(r *http.Request) (any, error) {
		return struct {
			Platform  string `json:"platform"`
			Partition string `json:"partition"`
			Domain    string `json:"domain"`
		}{Domain: "scw.cloud"}, nil
	})
	return ms
}

func (ms *mockServer) handle(w http.ResponseWriter, r *http.Request) {
	for prefix, handler := range ms.handlers {
		if strings.HasPrefix(r.URL.Path, prefix) {
			resp, err := handler(r)
			if err != nil {
				// If it's a ResponseError, write its status code.
				var respErr *scw.ResponseError
				if errors.As(err, &respErr) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(respErr.StatusCode)
					_ = json.NewEncoder(w).Encode(respErr)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
				return
			}
			if resp == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
	}
	ms.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
}

func (ms *mockServer) on(prefix string, handler func(r *http.Request) (any, error)) {
	ms.handlers[prefix] = handler
}

// statusHandler returns a fixed HTTP status code for a path prefix.
func (ms *mockServer) statusHandler(prefix string, code int) {
	ms.handlers[prefix] = func(r *http.Request) (any, error) {
		return nil, &scw.ResponseError{StatusCode: code, Status: http.StatusText(code)}
	}
}

// newMockClients builds a Clients struct backed by the mock server.
func newMockClients(t *testing.T, ms *mockServer, region scw.Region) *Clients {
	t.Helper()
	client, err := scw.NewClient(
		scw.WithAPIURL(ms.server.URL),
		scw.WithAuth("SCWXXXXXXXXXXXXXXXXX", "11111111-1111-1111-1111-111111111111"),
		scw.WithDefaultRegion(region),
		scw.WithDefaultZone(region.GetZones()[0]),
		scw.WithDefaultPageSize(50),
	)
	if err != nil {
		t.Fatalf("create scw client: %v", err)
	}

	awsConfig, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(string(region)),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("SCWXXXXXXXXXXXXXXXXX", "11111111-1111-1111-1111-111111111111", "")),
		awscfg.WithHTTPClient(ms.server.Client()),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}

	s3Client := s3sdk.NewFromConfig(awsConfig, func(o *s3sdk.Options) {
		o.BaseEndpoint = scw.StringPtr(ms.server.URL)
		o.UsePathStyle = true
	})

	return &Clients{
		scw:      client,
		account:  account.NewProjectAPI(client),
		instance: instance.NewAPI(client),
		lb:       lb.NewAPI(client),
		vpcgw:    vpcgw.NewAPI(client),
		s3:       s3Client,
		region:   region,
	}
}

// --- pure function tests ---

func TestFmtRAM(t *testing.T) {
	tests := []struct {
		input uint64
		want  string
	}{
		{512 * 1024 * 1024, "512 MiB"},
		{1024 * 1024 * 1024, "1 GiB"},
		{4 * 1024 * 1024 * 1024, "4 GiB"},
		{3*1024*1024*1024 + 512*1024*1024, "3.5 GiB"},
		{0, "0 MiB"},
	}
	for _, tt := range tests {
		got := fmtRAM(tt.input)
		if got != tt.want {
			t.Errorf("fmtRAM(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFmtBandwidth(t *testing.T) {
	tests := []struct {
		input uint64
		want  string
	}{
		{500 * 1_000_000, "500 Mbps"},
		{1_000_000_000, "1.0 Gbps"},
		{2_500_000_000, "2.5 Gbps"},
		{0, "0 Mbps"},
	}
	for _, tt := range tests {
		got := fmtBandwidth(tt.input)
		if got != tt.want {
			t.Errorf("fmtBandwidth(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFmtAvailability(t *testing.T) {
	tests := []struct {
		input instance.ServerTypesAvailability
		want  string
	}{
		{instance.ServerTypesAvailabilityAvailable, "🟢"},
		{instance.ServerTypesAvailabilityScarce, "🟠"},
		{instance.ServerTypesAvailabilityShortage, "🔴"},
		{"", "—"},
	}
	for _, tt := range tests {
		got := fmtAvailability(tt.input)
		if got != tt.want {
			t.Errorf("fmtAvailability(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFmtTime(t *testing.T) {
	if got := fmtTime(nil); got != "-" {
		t.Errorf("fmtTime(nil) = %q, want %q", got, "-")
	}
	ts := time.Date(2025, 3, 15, 14, 30, 0, 0, time.UTC)
	if got := fmtTime(&ts); got != "2025-03-15 14:30" {
		t.Errorf("fmtTime = %q, want %q", got, "2025-03-15 14:30")
	}
}

func TestPtrStr(t *testing.T) {
	if got := ptrStr(nil); got != "none" {
		t.Errorf("ptrStr(nil) = %q, want %q", got, "none")
	}
	s := "hello"
	if got := ptrStr(&s); got != "hello" {
		t.Errorf("ptrStr = %q, want %q", got, "hello")
	}
}

func TestIsNotImplemented(t *testing.T) {
	if isNotImplemented(nil) {
		t.Error("isNotImplemented(nil) = true, want false")
	}
	if !isNotImplemented(&scw.ResponseError{StatusCode: 501}) {
		t.Error("isNotImplemented(501) = false, want true")
	}
	if isNotImplemented(&scw.ResponseError{StatusCode: 500}) {
		t.Error("isNotImplemented(500) = true, want false")
	}
}

func TestResourceKindString(t *testing.T) {
	tests := []struct {
		kind ResourceKind
		want string
	}{
		{KindInstances, "instances"},
		{KindImages, "images"},
		{KindBuckets, "buckets"},
		{KindLoadBalancers, "loadbalancers"},
		{KindGateways, "gateways"},
		{KindSKUs, "skus"},
		{ResourceKind(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("Kind(%d).String() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestResourceKindHeader(t *testing.T) {
	tests := []struct {
		kind ResourceKind
		want []string
	}{
		{KindInstances, []string{"Name", "ID", "Type", "State", "Zone", "Public IP", "Private IP", "Created"}},
		{KindImages, []string{"Name", "ID", "Arch", "State", "Public", "Zone", "Created"}},
		{KindBuckets, []string{"Name", "Region", "Created"}},
		{KindLoadBalancers, []string{"Name", "ID", "Status", "Type", "Zone", "IP", "Frontends", "Backends", "Created"}},
		{KindGateways, []string{"Name", "ID", "Status", "Type", "Zone", "IP", "Created"}},
		{KindSKUs, []string{"Name", "Arch", "CPUs", "RAM", "GPU", "Hourly Price", "Zone", "SKU"}},
		{ResourceKind(99), nil},
	}
	for _, tt := range tests {
		got := tt.kind.Header()
		if len(got) != len(tt.want) {
			t.Errorf("Kind(%d).Header() len = %d, want %d", tt.kind, len(got), len(tt.want))
			continue
		}
		for i, h := range tt.want {
			if got[i] != h {
				t.Errorf("Kind(%d).Header()[%d] = %q, want %q", tt.kind, i, got[i], h)
			}
		}
	}
}

// --- config tests ---

func TestDefaultConfig(t *testing.T) {
	cfg := defaultConfig()
	if cfg.RefreshIntervalSeconds != defaultRefreshInterval {
		t.Errorf("RefreshIntervalSeconds = %d, want %d", cfg.RefreshIntervalSeconds, defaultRefreshInterval)
	}
	if cfg.DefaultRegion != string(scw.RegionFrPar) {
		t.Errorf("DefaultRegion = %q, want %q", cfg.DefaultRegion, scw.RegionFrPar)
	}
	if cfg.AccentColor != "#8344E5" {
		t.Errorf("AccentColor = %q, want #8344E5", cfg.AccentColor)
	}
}

func TestLoadConfigValid(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	content := `{"refreshIntervalSeconds": 10, "defaultRegion": "nl-ams", "accentColor": "#FF0000"}`
	if err := writeFile("config.json", content); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig()
	if cfg.RefreshIntervalSeconds != 10 {
		t.Errorf("RefreshIntervalSeconds = %d, want 10", cfg.RefreshIntervalSeconds)
	}
	if cfg.DefaultRegion != "nl-ams" {
		t.Errorf("DefaultRegion = %q, want nl-ams", cfg.DefaultRegion)
	}
	if cfg.AccentColor != "#FF0000" {
		t.Errorf("AccentColor = %q, want #FF0000", cfg.AccentColor)
	}
}

func TestLoadConfigMissing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	cfg := loadConfig()
	if cfg.RefreshIntervalSeconds != defaultRefreshInterval {
		t.Errorf("RefreshIntervalSeconds = %d, want %d", cfg.RefreshIntervalSeconds, defaultRefreshInterval)
	}
	if cfg.DefaultRegion != string(scw.RegionFrPar) {
		t.Errorf("DefaultRegion = %q, want %q", cfg.DefaultRegion, scw.RegionFrPar)
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := writeFile("config.json", "{invalid json}"); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig()
	if cfg.RefreshIntervalSeconds != defaultRefreshInterval {
		t.Errorf("RefreshIntervalSeconds = %d, want %d", cfg.RefreshIntervalSeconds, defaultRefreshInterval)
	}
}

func TestLoadConfigInvalidRegion(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	content := `{"defaultRegion": "invalid-region"}`
	if err := writeFile("config.json", content); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig()
	if cfg.DefaultRegion != string(scw.RegionFrPar) {
		t.Errorf("DefaultRegion = %q, want %q", cfg.DefaultRegion, scw.RegionFrPar)
	}
}

func TestLoadConfigZeroRefresh(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	content := `{"refreshIntervalSeconds": 0}`
	if err := writeFile("config.json", content); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig()
	if cfg.RefreshIntervalSeconds != defaultRefreshInterval {
		t.Errorf("RefreshIntervalSeconds = %d, want %d", cfg.RefreshIntervalSeconds, defaultRefreshInterval)
	}
}

// --- integration tests with mock API server ---

func TestFetchInstances(t *testing.T) {
	created := time.Date(2025, 1, 10, 8, 0, 0, 0, time.UTC)
	privIP := "10.0.0.1"
	ms := newMockServer(t)
	ms.on("/instance/v1/zones/", func(r *http.Request) (any, error) {
		// Only return data for fr-par-1; empty for other zones.
		if !strings.Contains(r.URL.Path, "fr-par-1") {
			return instance.ListServersResponse{Servers: []*instance.Server{}}, nil
		}
		return instance.ListServersResponse{
			Servers: []*instance.Server{
				{
					ID:             "srv-1",
					Name:           "web-1",
					CommercialType: "DEV1-S",
					State:          instance.ServerStateRunning,
					Zone:           scw.ZoneFrPar1,
					PrivateIP:      &privIP,
					PublicIPs:      []*instance.ServerIP{{Address: netParseIP("1.2.3.4"), State: instance.ServerIPStateAttached}},
					CreationDate:   &created,
				},
			},
		}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	rows, err := fetchInstances(c, nil)
	if err != nil {
		t.Fatalf("fetchInstances: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.ID != "srv-1" {
		t.Errorf("ID = %q, want srv-1", r.ID)
	}
	wantCols := []string{"web-1", "srv-1", "DEV1-S", "running", "fr-par-1", "1.2.3.4", "10.0.0.1", "2025-01-10 08:00"}
	for i, w := range wantCols {
		if i >= len(r.Cols) {
			t.Errorf("missing col %d", i)
			continue
		}
		if r.Cols[i] != w {
			t.Errorf("Cols[%d] = %q, want %q", i, r.Cols[i], w)
		}
	}
}

func TestFetchInstancesEmpty(t *testing.T) {
	ms := newMockServer(t)
	ms.on("/instance/v1/zones/", func(r *http.Request) (any, error) {
		return instance.ListServersResponse{Servers: []*instance.Server{}}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	rows, err := fetchInstances(c, nil)
	if err != nil {
		t.Fatalf("fetchInstances: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}

func TestFetchInstancesError(t *testing.T) {
	ms := newMockServer(t)
	ms.statusHandler("/instance/v1/zones/", 500)

	c := newMockClients(t, ms, scw.RegionFrPar)
	_, err := fetchInstances(c, nil)
	if err == nil {
		t.Error("fetchInstances with 500: expected error")
	}
}

func TestFetchInstancesWithProjectFilter(t *testing.T) {
	var mu sync.Mutex
	var seenProject string
	ms := newMockServer(t)
	ms.on("/instance/v1/zones/", func(r *http.Request) (any, error) {
		if pid := r.URL.Query().Get("project"); pid != "" {
			mu.Lock()
			seenProject = pid
			mu.Unlock()
		}
		return instance.ListServersResponse{Servers: []*instance.Server{}}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	pid := "proj-123"
	_, err := fetchInstances(c, &pid)
	if err != nil {
		t.Fatalf("fetchInstances: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenProject != "proj-123" {
		t.Errorf("project filter not sent, got %q", seenProject)
	}
}

func TestFetchImages(t *testing.T) {
	created := time.Date(2025, 2, 1, 12, 0, 0, 0, time.UTC)
	ms := newMockServer(t)
	ms.on("/instance/v1/zones/", func(r *http.Request) (any, error) {
		if !strings.Contains(r.URL.Path, "fr-par-2") {
			return instance.ListImagesResponse{Images: []*instance.Image{}}, nil
		}
		return instance.ListImagesResponse{
			Images: []*instance.Image{
				{
					ID:           "img-1",
					Name:         "ubuntu-24.04",
					Arch:         instance.ArchX86_64,
					State:        instance.ImageStateAvailable,
					Public:       true,
					Zone:         scw.ZoneFrPar2,
					CreationDate: &created,
				},
			},
		}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	rows, err := fetchImages(c, nil)
	if err != nil {
		t.Fatalf("fetchImages: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.ID != "img-1" {
		t.Errorf("ID = %q, want img-1", r.ID)
	}
	wantCols := []string{"ubuntu-24.04", "img-1", "x86_64", "available", "true", "fr-par-2", "2025-02-01 12:00"}
	for i, w := range wantCols {
		if i >= len(r.Cols) {
			t.Errorf("missing col %d", i)
			continue
		}
		if r.Cols[i] != w {
			t.Errorf("Cols[%d] = %q, want %q", i, r.Cols[i], w)
		}
	}
}

func TestFetchBuckets(t *testing.T) {
	// S3 ListBuckets returns XML; use a dedicated XML server.
	xmlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult>
  <Owner><ID>test</ID><DisplayName>test</DisplayName></Owner>
  <Buckets>
    <Bucket>
      <Name>my-bucket</Name>
      <CreationDate>2025-03-10T09:00:00.000Z</CreationDate>
    </Bucket>
  </Buckets>
</ListAllMyBucketsResult>`))
	}))
	t.Cleanup(xmlServer.Close)

	region := scw.RegionFrPar
	awsConfig, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(string(region)),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test-access", "test-secret", "")),
		awscfg.WithHTTPClient(xmlServer.Client()),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	s3Client := s3sdk.NewFromConfig(awsConfig, func(o *s3sdk.Options) {
		o.BaseEndpoint = scw.StringPtr(xmlServer.URL)
		o.UsePathStyle = true
	})

	c := &Clients{
		s3:     s3Client,
		region: region,
	}
	rows, err := fetchBuckets(c, nil)
	if err != nil {
		t.Fatalf("fetchBuckets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.ID != "my-bucket" {
		t.Errorf("ID = %q, want my-bucket", r.ID)
	}
	if r.Cols[0] != "my-bucket" {
		t.Errorf("Name = %q, want my-bucket", r.Cols[0])
	}
	if r.Cols[1] != "fr-par" {
		t.Errorf("Region = %q, want fr-par", r.Cols[1])
	}
}

func TestFetchLoadBalancers(t *testing.T) {
	created := time.Date(2025, 4, 5, 10, 0, 0, 0, time.UTC)
	ms := newMockServer(t)
	ms.on("/lb/v1/regions/", func(r *http.Request) (any, error) {
		// Only return data for fr-par; return 501 for other regions.
		if !strings.Contains(r.URL.Path, "fr-par") {
			return nil, &scw.ResponseError{StatusCode: 501}
		}
		return lb.ListLBsResponse{
			LBs: []*lb.LB{
				{
					ID:            "lb-1",
					Name:          "my-lb",
					Status:        lb.LBStatusReady,
					Type:          "LB-S",
					Zone:          scw.ZoneFrPar1,
					IP:            []*lb.IP{{IPAddress: "51.1.1.1"}},
					FrontendCount: 2,
					BackendCount:  3,
					CreatedAt:     &created,
				},
			},
		}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	rows, err := fetchLoadBalancers(c, nil)
	if err != nil {
		t.Fatalf("fetchLoadBalancers: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.ID != "lb-1" {
		t.Errorf("ID = %q, want lb-1", r.ID)
	}
	wantCols := []string{"my-lb", "lb-1", "ready", "LB-S", "fr-par-1", "51.1.1.1", "2", "3", "2025-04-05 10:00"}
	for i, w := range wantCols {
		if i >= len(r.Cols) {
			t.Errorf("missing col %d", i)
			continue
		}
		if r.Cols[i] != w {
			t.Errorf("Cols[%d] = %q, want %q", i, r.Cols[i], w)
		}
	}
}

func TestFetchGateways(t *testing.T) {
	created := time.Date(2025, 5, 15, 14, 0, 0, 0, time.UTC)
	ms := newMockServer(t)
	ms.on("/vpc-gw/v1/zones/", func(r *http.Request) (any, error) {
		if !strings.Contains(r.URL.Path, "fr-par-1") {
			return nil, &scw.ResponseError{StatusCode: 501}
		}
		return vpcgw.ListGatewaysResponse{
			Gateways: []*vpcgw.Gateway{
				{
					ID:        "gw-1",
					Name:      "my-gw",
					Status:    vpcgw.GatewayStatusRunning,
					Type:      &vpcgw.GatewayType{Name: "VPC-GW-S"},
					Zone:      scw.ZoneFrPar1,
					IP:        &vpcgw.IP{Address: netParseIP("52.2.2.2")},
					CreatedAt: &created,
				},
			},
		}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	rows, err := fetchGateways(c, nil)
	if err != nil {
		t.Fatalf("fetchGateways: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.ID != "gw-1" {
		t.Errorf("ID = %q, want gw-1", r.ID)
	}
	wantCols := []string{"my-gw", "gw-1", "running", "VPC-GW-S", "fr-par-1", "52.2.2.2", "2025-05-15 14:00"}
	for i, w := range wantCols {
		if i >= len(r.Cols) {
			t.Errorf("missing col %d", i)
			continue
		}
		if r.Cols[i] != w {
			t.Errorf("Cols[%d] = %q, want %q", i, r.Cols[i], w)
		}
	}
}

func TestFetchSKUs(t *testing.T) {
	gpu := uint64(0)
	gpuCount := uint64(1)
	ms := newMockServer(t)
	ms.on("/instance/v1/zones/", func(r *http.Request) (any, error) {
		if strings.HasSuffix(r.URL.Path, "/products/servers/availability") {
			return instance.GetServerTypesAvailabilityResponse{
				Servers: map[string]*instance.GetServerTypesAvailabilityResponseAvailability{
					"DEV1-S": {Availability: instance.ServerTypesAvailabilityAvailable},
				},
			}, nil
		}
		// ListServersTypes
		return instance.ListServersTypesResponse{
			Servers: map[string]*instance.ServerType{
				"DEV1-S": {
					Arch:        instance.ArchX86_64,
					Ncpus:       2,
					RAM:         4 * 1024 * 1024 * 1024,
					Gpu:         &gpuCount,
					HourlyPrice: 0.01,
					Sku:         "dev1-s",
				},
				"GP1-XS": {
					Arch:        instance.ArchX86_64,
					Ncpus:       4,
					RAM:         16 * 1024 * 1024 * 1024,
					Gpu:         &gpu,
					HourlyPrice: 0.10,
				},
			},
		}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	zones := scw.RegionFrPar.GetZones()
	rows, err := fetchSKUs(c, scw.RegionFrPar, zones)
	if err != nil {
		t.Fatalf("fetchSKUs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	// Find rows by name (map order is random).
	byName := make(map[string]Row, len(rows))
	for _, r := range rows {
		byName[r.Cols[0]] = r
	}

	if dev, ok := byName["DEV1-S"]; ok {
		want := []string{"DEV1-S", "x86_64", "2", "4 GiB", "1"}
		for i, w := range want {
			if dev.Cols[i] != w {
				t.Errorf("DEV1-S Cols[%d] = %q, want %q", i, dev.Cols[i], w)
			}
		}
		// After 5 base cols, zone availability columns follow.
		for i, z := range zones {
			idx := 5 + i
			if dev.Cols[idx] != "🟢" {
				t.Errorf("DEV1-S availability for %s = %q, want 🟢", z, dev.Cols[idx])
			}
		}
	} else {
		t.Error("DEV1-S row not found")
	}

	if gp, ok := byName["GP1-XS"]; ok {
		if gp.Cols[4] != "-" {
			t.Errorf("GP1-XS GPU = %q, want -", gp.Cols[4])
		}
	} else {
		t.Error("GP1-XS row not found")
	}
}

func TestFetchInstanceDetail(t *testing.T) {
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	modified := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	ms := newMockServer(t)
	ms.on("/instance/v1/zones/", func(r *http.Request) (any, error) {
		return instance.GetServerResponse{
			Server: &instance.Server{
				ID:               "srv-detail-1",
				Name:             "web-1",
				State:            instance.ServerStateRunning,
				StateDetail:      "running normally",
				CommercialType:   "DEV1-S",
				Arch:             instance.ArchX86_64,
				Zone:             scw.ZoneFrPar1,
				Hostname:         "web-1",
				BootType:         instance.BootTypeLocal,
				Organization:     "org-1",
				Project:          "proj-1",
				Tags:             []string{"tag1", "tag2"},
				CreationDate:     &created,
				ModificationDate: &modified,
				Protected:        false,
			},
		}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	detail, err := fetchInstanceDetail(c, "srv-detail-1", "fr-par-1")
	if err != nil {
		t.Fatalf("fetchInstanceDetail: %v", err)
	}
	if len(detail) == 0 {
		t.Fatal("got empty detail")
	}
	// Check first field is Name.
	if detail[0] != "Name: web-1" {
		t.Errorf("detail[0] = %q, want %q", detail[0], "Name: web-1")
	}
	// Find State field.
	found := false
	for _, d := range detail {
		if d == "State: running" {
			found = true
			break
		}
	}
	if !found {
		t.Error("State: running not found in detail")
	}
}

func TestFetchInstanceDetailNotFound(t *testing.T) {
	ms := newMockServer(t)
	ms.statusHandler("/instance/v1/zones/", 404)

	c := newMockClients(t, ms, scw.RegionFrPar)
	_, err := fetchInstanceDetail(c, "nonexistent", "fr-par-1")
	if err == nil {
		t.Error("fetchInstanceDetail with 404: expected error")
	}
}

func TestFetchSKUDetail(t *testing.T) {
	gpuCount := uint64(1)
	monthlyPrice := float32(10.0)
	ms := newMockServer(t)
	ms.on("/instance/v1/zones/", func(r *http.Request) (any, error) {
		if strings.HasSuffix(r.URL.Path, "/products/servers/availability") {
			return instance.GetServerTypesAvailabilityResponse{
				Servers: map[string]*instance.GetServerTypesAvailabilityResponseAvailability{
					"DEV1-S": {Availability: instance.ServerTypesAvailabilityAvailable},
				},
			}, nil
		}
		return instance.ListServersTypesResponse{
			Servers: map[string]*instance.ServerType{
				"DEV1-S": {
					Sku:          "dev1-s",
					Arch:         instance.ArchX86_64,
					Ncpus:        2,
					RAM:          4 * 1024 * 1024 * 1024,
					HourlyPrice:  0.01,
					MonthlyPrice: &monthlyPrice,
					Gpu:          &gpuCount,
					EndOfService: true,
				},
			},
		}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	detail, err := fetchSKUDetail(c, "DEV1-S")
	if err != nil {
		t.Fatalf("fetchSKUDetail: %v", err)
	}
	if len(detail.Specs) == 0 {
		t.Fatal("got empty specs")
	}
	expectSpecs := []string{
		"Name: DEV1-S",
		"CPUs: 2",
		"RAM: 4 GiB",
		"GPUs: 1",
		"End of Service: true",
	}
	for _, want := range expectSpecs {
		found := false
		for _, s := range detail.Specs {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("specs missing %q; got: %v", want, detail.Specs)
		}
	}

	if len(detail.ZoneRows) != len(scw.AllZones) {
		t.Fatalf("got %d zone rows, want %d", len(detail.ZoneRows), len(scw.AllZones))
	}
	for _, row := range detail.ZoneRows {
		if len(row) != 4 {
			t.Errorf("zone row has %d cols, want 4: %v", len(row), row)
		}
		if row[1] != "€0.0100" {
			t.Errorf("hourly price for %s = %q, want €0.0100", row[0], row[1])
		}
		if row[2] != "€10.00" {
			t.Errorf("monthly price for %s = %q, want €10.00", row[0], row[2])
		}
		if row[3] != "🟢" {
			t.Errorf("availability for %s = %q, want 🟢", row[0], row[3])
		}
	}
}

func TestFetchSKUDetailNotFound(t *testing.T) {
	ms := newMockServer(t)
	ms.on("/instance/v1/zones/", func(r *http.Request) (any, error) {
		return instance.ListServersTypesResponse{Servers: map[string]*instance.ServerType{}}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	_, err := fetchSKUDetail(c, "nonexistent-type")
	if err == nil {
		t.Error("fetchSKUDetail with missing type: expected error")
	}
}

func TestFetchProjects(t *testing.T) {
	ms := newMockServer(t)
	ms.on("/account/v3/projects", func(r *http.Request) (any, error) {
		return account.ListProjectsResponse{
			Projects: []*account.Project{
				{ID: "proj-1", Name: "Production"},
				{ID: "proj-2", Name: "Staging"},
			},
		}, nil
	})

	c := newMockClients(t, ms, scw.RegionFrPar)
	projects, err := fetchProjects(c)
	if err != nil {
		t.Fatalf("fetchProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(projects))
	}
	if projects[0].ID != "proj-1" || projects[0].Name != "Production" {
		t.Errorf("projects[0] = {%s, %s}, want {proj-1, Production}", projects[0].ID, projects[0].Name)
	}
	if projects[1].ID != "proj-2" || projects[1].Name != "Staging" {
		t.Errorf("projects[1] = {%s, %s}, want {proj-2, Staging}", projects[1].ID, projects[1].Name)
	}
}

func TestFetchProjectsError(t *testing.T) {
	ms := newMockServer(t)
	ms.statusHandler("/account/v3/projects", 500)

	c := newMockClients(t, ms, scw.RegionFrPar)
	_, err := fetchProjects(c)
	if err == nil {
		t.Error("fetchProjects with 500: expected error")
	}
}

// --- helpers ---

// netParseIP wraps net.ParseIP for convenience in test data construction.
func netParseIP(s string) net.IP {
	return net.ParseIP(s)
}

func writeFile(name, content string) error {
	return os.WriteFile(name, []byte(content), 0644)
}
