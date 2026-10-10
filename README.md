# scw-tui

Terminal UI for Scaleway cloud resources, inspired by k9s. Read-only prototype.

## Prerequisites

- Go 1.26+
- Scaleway credentials configured via `~/.scwrc` or environment variables (`SCW_ACCESS_KEY`, `SCW_SECRET_KEY`, `SCW_DEFAULT_REGION`)

Run `scw init` to set up credentials if you haven't already.

## Build

```sh
go build -o scw-tui .
```

## Run

```sh
./scw-tui
```

## Configuration

A `config.json` file in the working directory controls TUI behavior:

```json
{
  "refreshIntervalSeconds": 5,
  "defaultRegion": "fr-par",
  "accentColor": "#8344E5"
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `refreshIntervalSeconds` | `5` | Auto-refresh interval in seconds |
| `defaultRegion` | `fr-par` | Default region for SKU availability view |
| `accentColor` | `#8344E5` | Accent color (hex) for headers, first column, highlights |

## Keybindings

| Key | Action |
|-----|--------|
| `1`–`6` | Switch resource: instances, images, buckets, load balancers, gateways, SKUs |
| `Tab` / `Shift+Tab` | Cycle to next/previous resource |
| `r` | Refresh current view |
| `p` | Select project filter (k9s-style context switcher) |
| `R` | Select region for SKU availability view (only in SKUs view) |
| `S` | Select column to sort by |
| `/` | Search/filter rows (incremental, Enter to apply, Escape to cancel) |
| `Enter` | Show detail for selected row |
| `q` | Quit |

## Resources

- **Instances** — server instances across all zones (ID, name, type, state, zone, IPs, created)
- **Images** — instance images across all zones (ID, name, arch, state, public, zone, created)
- **Buckets** — S3 object storage buckets (name, region, created)
- **Load Balancers** — all LBs across all regions (ID, name, status, type, zone, IPs, frontend/backend counts, created)
- **Public Gateways** — VPC gateways across all zones (ID, name, status, type, zone, IP, created)
- **SKUs** — instance server types per region (name, arch, CPUs, RAM, GPU, availability per zone). `R` switches region, `Enter` shows full specs (price, GPU model, network bandwidth, capabilities, end-of-service).
