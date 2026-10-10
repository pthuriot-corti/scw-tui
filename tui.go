package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

// accentColor is the accent used for headers, first column, and highlights.
var accentColor = tcell.NewHexColor(0x8344E5)

// accentTag is the tview color tag for the accent color.
var accentTag = "[#8344E5]"
var accentBoldTag = "[#8344E5::b]"

// App holds the TUI state.
type App struct {
	clients *Clients
	view    *tview.Application
	layout  *tview.Flex
	table   *tview.Table
	status  *tview.TextView
	kind    ResourceKind
	rows    []Row
	config  Config

	projectID   *string // nil = all projects
	projectName string  // display name
	projects    []ProjectInfo

	skuRegion scw.Region // region for SKU availability view
	skuZones  []scw.Zone // zones in current SKU region

	sortCols map[ResourceKind]int // per-kind sort column, -1 = none

	// Search state
	allRows      []Row  // unfiltered rows before search
	searching    bool   // search bar is open
	searchFilter string // active search text, "" = no filter
	searchBar    *tview.InputField
	// SKU detail state
	skuDetailType string // current detail server type name

	stopAutoRefresh chan struct{}
}

var kindOrder = []ResourceKind{
	KindSKUs,
	KindInstances,
	KindImages,
	KindBuckets,
	KindLoadBalancers,
	KindGateways,
}

func newApp(clients *Clients, cfg Config) *App {
	a := &App{
		clients:         clients,
		config:          cfg,
		skuRegion:       scw.Region(cfg.DefaultRegion),
		sortCols:        make(map[ResourceKind]int),
		stopAutoRefresh: make(chan struct{}),
	}
	a.skuZones = a.skuRegion.GetZones()

	// Override accent color from config.
	if a.config.AccentColor != "" {
		hex := strings.TrimPrefix(a.config.AccentColor, "#")
		if v, err := strconv.ParseUint(hex, 16, 32); err == nil {
			accentColor = tcell.NewHexColor(int32(v))
			accentTag = "[#" + strings.ToUpper(hex) + "]"
			accentBoldTag = "[#" + strings.ToUpper(hex) + "::b]"
		}
	}

	// Start on the default project if one is configured.
	if clients.defaultProjectID != "" {
		pid := clients.defaultProjectID
		a.projectID = &pid
		a.projectName = clients.defaultProjectName
	}

	a.table = tview.NewTable().
		SetSelectable(true, false).
		SetSeparator(tview.Borders.Vertical).
		SetFixed(1, 0)
	a.table.SetBackgroundColor(tcell.ColorDefault)
	a.table.SetTitleAlign(tview.AlignLeft)

	a.status = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	a.status.SetBackgroundColor(tcell.Color236)
	a.status.SetTextColor(tcell.ColorWhite)

	a.searchBar = tview.NewInputField().
		SetLabel("/").
		SetLabelColor(accentColor).
		SetFieldTextColor(tcell.ColorWhite).
		SetFieldBackgroundColor(tcell.Color236)
	a.searchBar.SetBackgroundColor(tcell.Color236)
	a.searchBar.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			a.applySearch()
		case tcell.KeyEsc:
			a.closeSearch()
		}
	})
	a.searchBar.SetChangedFunc(func(text string) {
		a.filterSearch(text)
	})

	a.layout = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(a.table, 0, 1, true).
		AddItem(a.searchBar, 0, 0, false).
		AddItem(a.status, 1, 0, false)

	a.view = tview.NewApplication().SetRoot(a.layout, true)
	a.view.SetFocus(a.table)

	a.setKeybindings()
	return a
}

func (a *App) setKeybindings() {
	a.table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyRune:
			switch event.Rune() {
			case 'r':
				a.refresh()
				return nil
			case '/':
				a.startSearch()
				return nil
			case 'R':
				if a.kind == KindSKUs {
					a.showRegionSelector()
				}
				return nil
			case 'A':
				if a.kind == KindSKUs {
					a.showAllAvailability()
				}
				return nil
			case 'S':
				a.showSortSelector()
				return nil
			case 'p':
				a.showProjectSelector()
				return nil
			case '1', '2', '3', '4', '5', '6':
				idx := int(event.Rune() - '1')
				if idx < len(kindOrder) {
					a.switchKind(kindOrder[idx])
				}
				return nil
			case 'q':
				a.view.Stop()
				return nil
			}
		case tcell.KeyTab:
			a.cycleKind(1)
			return nil
		case tcell.KeyBacktab:
			a.cycleKind(-1)
			return nil
		case tcell.KeyEnter:
			a.showDetail()
			return nil
		case tcell.KeyEsc:
			if a.searchFilter != "" {
				a.closeSearch()
				return nil
			}
		}
		return event
	})
}

func (a *App) cycleKind(dir int) {
	cur := -1
	for i, k := range kindOrder {
		if k == a.kind {
			cur = i
			break
		}
	}
	if cur == -1 {
		a.switchKind(kindOrder[0])
		return
	}
	next := (cur + dir + len(kindOrder)) % len(kindOrder)
	a.switchKind(kindOrder[next])
}

func (a *App) switchKind(kind ResourceKind) {
	if a.searching {
		a.searching = false
		a.layout.ResizeItem(a.searchBar, 0, 0)
	}
	a.searchFilter = ""
	a.kind = kind
	a.refresh()
}

func (a *App) refresh() {
	go func() {
		rows, err := a.fetch()
		a.view.QueueUpdateDraw(func() {
			if err != nil {
				a.setStatus("[red]error: %s[-]", err)
				return
			}
			a.rows = rows
			a.allRows = rows
			if a.searching {
				a.filterSearch(a.searchBar.GetText())
			} else if a.searchFilter != "" {
				a.applySearchFilter(a.searchFilter)
			} else {
				a.renderTable()
			}
			a.setStatus(accentBoldTag+"%s[-] [white]|[-] %s%s: %d items — [white]1[-] skus [white]2[-] instances [white]3[-] images [white]4[-] buckets [white]5[-] lbs [white]6[-] gateways — [white]p[-] project [white]R[-] region [white]A[-] all zones [white]S[-] sort [white]/[-] search [white]r[-] refresh [white]Tab[-] next [white]Enter[-] detail [white]q[-] quit",
				a.projectLabel(), a.kind, a.skuRegionSuffix(), len(a.rows))
		})
	}()
}

func (a *App) fetch() ([]Row, error) {
	switch a.kind {
	case KindInstances:
		return fetchInstances(a.clients, a.projectID)
	case KindImages:
		return fetchImages(a.clients, a.projectID)
	case KindBuckets:
		return fetchBuckets(a.clients, a.projectID)
	case KindLoadBalancers:
		return fetchLoadBalancers(a.clients, a.projectID)
	case KindGateways:
		return fetchGateways(a.clients, a.projectID)
	case KindSKUs:
		return fetchSKUs(a.clients, a.skuRegion, a.skuZones)
	default:
		return nil, fmt.Errorf("unknown resource kind")
	}
}

func (a *App) renderTable() {
	// Sort rows if a sort column is set.
	a.sortRows()

	// Preserve scroll position across re-renders.
	prevRow, _ := a.table.GetSelection()
	a.table.Clear()
	header := a.skuHeader()
	for col, h := range header {
		cell := tview.NewTableCell(" " + strings.ToUpper(h) + " ").
			SetSelectable(false).
			SetTextColor(accentColor).
			SetAttributes(tcell.AttrBold).
			SetExpansion(1)
		a.table.SetCell(0, col, cell)
	}
	for r, row := range a.rows {
		for c, val := range row.Cols {
			cell := tview.NewTableCell(" " + val + " ").
				SetExpansion(1)
			if c == 0 {
				cell.SetTextColor(accentColor)
			}
			a.table.SetCell(r+1, c, cell)
		}
	}
	// Restore selection if still valid, otherwise select first data row.
	if len(a.rows) > 0 {
		if prevRow >= 1 && prevRow <= len(a.rows) {
			a.table.Select(prevRow, 0)
		} else {
			a.table.Select(1, 0)
		}
	}
	a.table.SetTitle(fmt.Sprintf(" %s %s ", strings.ToUpper(a.kind.String()), a.skuTitleSuffix()))
}

// sortColumn returns the sort column for the current kind (-1 = none).
func (a *App) sortColumn() int {
	if c, ok := a.sortCols[a.kind]; ok {
		return c
	}
	return 0
}

// sortRows sorts a.rows by sort column if set. Numeric prefixes compared numerically.
func (a *App) sortRows() {
	col := a.sortColumn()
	if col < 0 || col >= len(a.skuHeader()) {
		return
	}
	sort.SliceStable(a.rows, func(i, j int) bool {
		return cellLess(a.rows[i].Cols[col], a.rows[j].Cols[col])
	})
}

// cellLess compares two cell strings, numeric prefix first, then lexicographic.
func cellLess(a, b string) bool {
	na, okA := parseNumPrefix(a)
	nb, okB := parseNumPrefix(b)
	if okA && okB {
		if na != nb {
			return na < nb
		}
	}
	return a < b
}

// parseNumPrefix extracts a leading number (int or float) from a cell string.
func parseNumPrefix(s string) (float64, bool) {
	s = strings.TrimLeft(s, " ")
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// showSortSelector opens a list dialog to pick which column to sort by.
func (a *App) showSortSelector() {
	header := a.skuHeader()
	cur := a.sortColumn()
	list := tview.NewList().
		ShowSecondaryText(false)
	list.SetBorder(true).
		SetTitle(" Sort By Column ").
		SetBackgroundColor(tcell.Color236)

	list.AddItem(accentTag+"]No sort[-]", "", '0', func() {
		a.sortCols[a.kind] = -1
		a.view.SetRoot(a.layout, true).SetFocus(a.table)
		a.renderTable()
	})

	for i, h := range header {
		idx := i
		label := strings.ToUpper(h)
		if idx == cur {
			label = accentBoldTag + label + " (current)[-]"
		}
		runeKey := rune('a' + i)
		if runeKey > 'z' {
			runeKey = 'z'
		}
		list.AddItem(label, "", runeKey, func() {
			a.sortCols[a.kind] = idx
			a.view.SetRoot(a.layout, true).SetFocus(a.table)
			a.renderTable()
		})
	}

	list.AddItem("[darkgray]Cancel[-]", "", 'q', func() {
		a.view.SetRoot(a.layout, true).SetFocus(a.table)
	})

	flex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(tview.NewBox(), 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(tview.NewBox(), 0, 1, false).
			AddItem(list, 0, 1, true).
			AddItem(tview.NewBox(), 0, 1, false), 0, 1, true).
		AddItem(tview.NewBox(), 0, 1, false)

	a.view.SetRoot(flex, true).SetFocus(list)
}

// startSearch saves current rows and shows the search input.
func (a *App) startSearch() {
	if a.searching {
		return
	}
	a.searching = true
	a.allRows = a.rows
	a.searchBar.SetText("")
	a.layout.ResizeItem(a.searchBar, 1, 0)
	a.view.SetFocus(a.searchBar)
}

// filterSearch filters the table rows as the user types.
func (a *App) filterSearch(text string) {
	if !a.searching {
		return
	}
	if text == "" {
		a.rows = a.allRows
	} else {
		lower := strings.ToLower(text)
		a.rows = make([]Row, 0, len(a.allRows))
		for _, r := range a.allRows {
			for _, c := range r.Cols {
				if strings.Contains(strings.ToLower(c), lower) {
					a.rows = append(a.rows, r)
					break
				}
			}
		}
	}
	a.renderTable()
}

// applySearchFilter filters allRows by text and renders. Used by auto-refresh.
func (a *App) applySearchFilter(text string) {
	if text == "" {
		a.rows = a.allRows
	} else {
		lower := strings.ToLower(text)
		a.rows = make([]Row, 0, len(a.allRows))
		for _, r := range a.allRows {
			for _, c := range r.Cols {
				if strings.Contains(strings.ToLower(c), lower) {
					a.rows = append(a.rows, r)
					break
				}
			}
		}
	}
	a.renderTable()
}

// applySearch keeps the search bar visible with the keyword, focus returns to table.
func (a *App) applySearch() {
	a.searching = false
	a.searchFilter = a.searchBar.GetText()
	a.view.SetFocus(a.table)
}

// closeSearch resets the search filter and hides the search bar.
func (a *App) closeSearch() {
	a.searching = false
	a.searchFilter = ""
	a.searchBar.SetText("")
	a.layout.ResizeItem(a.searchBar, 0, 0)
	a.view.SetFocus(a.table)
	a.rows = a.allRows
	a.renderTable()
}

// skuHeader returns the column headers, dynamic for the SKU view.
func (a *App) skuHeader() []string {
	if a.kind != KindSKUs {
		return a.kind.Header()
	}
	h := []string{"Name", "Arch", "CPUs", "RAM", "GPU"}
	for _, z := range a.skuZones {
		h = append(h, z.String())
	}
	return h
}

// skuTitleSuffix returns a region suffix for the SKU view title.
func (a *App) skuTitleSuffix() string {
	if a.kind != KindSKUs {
		return ""
	}
	return "[" + string(a.skuRegion) + "]"
}

// skuRegionSuffix returns a region label for the status bar.
func (a *App) skuRegionSuffix() string {
	if a.kind != KindSKUs {
		return ""
	}
	return " [" + string(a.skuRegion) + "]"
}

func (a *App) showDetail() {
	row, _ := a.table.GetSelection()
	if row < 1 || row > len(a.rows) {
		return
	}
	r := a.rows[row-1]

	// SKUs: fetch full specs from the API for the detail view.
	if a.kind == KindSKUs {
		a.skuDetailType = r.Cols[0]
		a.showSKUDetail(r.Cols[0])
	}

	// Instances: fetch full server details via GetServer API.
	if a.kind == KindInstances && len(r.Cols) >= 5 {
		a.showInstanceDetail(r.Cols[1], r.Cols[4]) // ID, zone
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, accentBoldTag+"%s[-]\n\n", strings.ToUpper(a.kind.String()))
	header := a.kind.Header()
	for i, h := range header {
		if i < len(r.Cols) {
			fmt.Fprintf(&sb, "  [cyan]%-12s[-] %s\n", h+":", r.Cols[i])
		}
	}
	a.showDetailModal(sb.String())
}

func (a *App) showInstanceDetail(serverID, zone string) {
	go func() {
		detail, err := fetchInstanceDetail(a.clients, serverID, zone)
		a.view.QueueUpdateDraw(func() {
			if err != nil {
				a.setStatus("[red]error: %s[-]", err)
				return
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, accentBoldTag+"INSTANCE: %s[-]\n\n", detail[0])
			for _, s := range detail[1:] {
				parts := strings.SplitN(s, ": ", 2)
				label := parts[0] + ":"
				val := ""
				if len(parts) > 1 {
					val = parts[1]
				}
				fmt.Fprintf(&sb, "  [cyan]%-20s[-] %s\n", label, val)
			}
			a.showDetailModal(sb.String())
		})
	}()
}

func (a *App) showSKUDetail(typeName string) {
	a.skuDetailType = typeName
	a.setStatus("loading specs for %s...", typeName)
	go func() {
		detail, err := fetchSKUDetail(a.clients, typeName)
		a.view.QueueUpdateDraw(func() {
			if err != nil {
				a.setStatus("[red]error: %s[-]", err)
				return
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, accentBoldTag+"SERVER TYPE: %s[-]\n\n", typeName)
			for _, s := range detail.Specs {
				parts := strings.SplitN(s, ": ", 2)
				label := parts[0] + ":"
				val := ""
				if len(parts) > 1 {
					val = parts[1]
				}
				fmt.Fprintf(&sb, "  [cyan]%-20s[-] %s\n", label, val)
			}
			sb.WriteString("\n" + accentBoldTag + "PRICING & AVAILABILITY[-]\n\n")
			fmt.Fprintf(&sb, "  [cyan]%-10s %-12s %-12s %s[-]\n", "Zone", "Hourly", "Monthly", "Availability")
			for _, row := range detail.ZoneRows {
				fmt.Fprintf(&sb, "  %-10s %-12s %-12s %s\n", row[0], row[1], row[2], row[3])
			}
			a.showDetailModal(sb.String())
		})
	}()
}

func (a *App) showDetailModal(text string) {
	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft).
		SetText(text)
	textView.SetBorder(true).
		SetBackgroundColor(tcell.Color236).
		SetTitle(" Detail ")

	closeBtn := tview.NewButton("Close").
		SetSelectedFunc(func() {
			a.view.SetRoot(a.layout, true).SetFocus(a.table)
		})
	closeBtn.SetBackgroundColor(tcell.Color236)

	flex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(textView, 0, 1, false).
		AddItem(closeBtn, 1, 0, true)

	closeDetail := func() {
		a.view.SetRoot(a.layout, true).SetFocus(a.table)
	}
	flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			closeDetail()
			return nil
		case tcell.KeyRune:
			if event.Rune() == 'q' {
				closeDetail()
				return nil
			}
		}
		return event
	})

	a.view.SetRoot(flex, true).SetFocus(closeBtn)
}

func (a *App) setStatus(format string, args ...interface{}) {
	a.status.SetText(fmt.Sprintf(" %s", fmt.Sprintf(format, args...)))
}

func (a *App) run() error {
	a.switchKind(KindSKUs)

	// Start auto-refresh ticker in background.
	go a.autoRefreshLoop()

	// Ensure auto-refresh stops when the app exits.
	defer close(a.stopAutoRefresh)

	return a.view.Run()
}

// autoRefreshLoop periodically refreshes the current view until stopped.
func (a *App) autoRefreshLoop() {
	interval := time.Duration(a.config.RefreshIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = time.Duration(defaultRefreshInterval) * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.view.QueueUpdateDraw(func() {
				a.refresh()
			})
		case <-a.stopAutoRefresh:
			return
		}
	}
}

// projectLabel returns the display name for the current project filter.
func (a *App) projectLabel() string {
	if a.projectName == "" {
		return "ALL PROJECTS"
	}
	return a.projectName
}

// showProjectSelector opens a list dialog to pick a project.
func (a *App) showProjectSelector() {
	a.setStatus("loading projects...")
	go func() {
		projects, err := fetchProjects(a.clients)
		a.view.QueueUpdateDraw(func() {
			if err != nil {
				a.setStatus("[red]error loading projects: %s[-]", err)
				return
			}
			a.projects = projects
			a.renderProjectSelector()
		})
	}()
}

func (a *App) renderProjectSelector() {
	list := tview.NewList().
		ShowSecondaryText(false)
	list.SetBorder(true).
		SetTitle(" Select Project ").
		SetBackgroundColor(tcell.Color236)

	// "All projects" option first.
	list.AddItem(accentTag+"]ALL PROJECTS[-]", "", 'a', func() {
		a.projectID = nil
		a.projectName = ""
		a.view.SetRoot(a.layout, true).SetFocus(a.table)
		a.refresh()
	})

	for i, p := range a.projects {
		name := p.Name
		id := p.ID
		runeKey := rune('a' + 1 + i)
		if runeKey > 'z' {
			runeKey = 'z'
		}
		list.AddItem(fmt.Sprintf("%s [darkgray](%s)[-]", name, id), "", runeKey, func() {
			a.projectID = &id
			a.projectName = name
			a.view.SetRoot(a.layout, true).SetFocus(a.table)
			a.refresh()
		})
	}

	list.AddItem("[darkgray]Cancel[-]", "", 'q', func() {
		a.view.SetRoot(a.layout, true).SetFocus(a.table)
	})

	flex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(tview.NewBox(), 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(tview.NewBox(), 0, 1, false).
			AddItem(list, 0, 1, true).
			AddItem(tview.NewBox(), 0, 1, false), 0, 1, true).
		AddItem(tview.NewBox(), 0, 1, false)

	a.view.SetRoot(flex, true).SetFocus(list)
}

// showRegionSelector opens a list dialog to pick a region for the SKU view.
func (a *App) showRegionSelector() {
	list := tview.NewList().
		ShowSecondaryText(false)
	list.SetBorder(true).
		SetTitle(" Select Region ").
		SetBackgroundColor(tcell.Color236)

	for i, region := range scw.AllRegions {
		region := region // capture
		label := string(region)
		if region == a.skuRegion {
			label = accentBoldTag + label + " (current)[-]"
		}
		runeKey := rune('a' + i)
		if runeKey > 'z' {
			runeKey = 'z'
		}
		list.AddItem(label, "", runeKey, func() {
			a.skuRegion = region
			a.skuZones = region.GetZones()
			a.view.SetRoot(a.layout, true).SetFocus(a.table)
			a.refresh()
		})
	}

	list.AddItem("[darkgray]Cancel[-]", "", 'q', func() {
		a.view.SetRoot(a.layout, true).SetFocus(a.table)
	})

	flex := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(tview.NewBox(), 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(tview.NewBox(), 0, 1, false).
			AddItem(list, 0, 1, true).
			AddItem(tview.NewBox(), 0, 1, false), 0, 1, true).
		AddItem(tview.NewBox(), 0, 1, false)

	a.view.SetRoot(flex, true).SetFocus(list)
}

// availabilityColor returns a tcell color for an availability emoji.
func availabilityColor(s string) tcell.Color {
	switch s {
	case "🟢":
		return tcell.ColorGreen
	case "🟠":
		return tcell.ColorOrange
	case "🔴":
		return tcell.ColorRed
	default:
		return tcell.ColorGray
	}
}

// showAllAvailability opens a table of all SKUs × all zones availability.
func (a *App) showAllAvailability() {
	a.setStatus("loading availability for all zones...")
	go func() {
		rows, err := fetchSKUs(a.clients, a.skuRegion, scw.AllZones)
		a.view.QueueUpdateDraw(func() {
			if err != nil {
				a.setStatus("[red]error: %s[-]", err)
				return
			}
			sort.SliceStable(rows, func(i, j int) bool {
				return rows[i].Cols[0] < rows[j].Cols[0]
			})
			table := tview.NewTable().
				SetFixed(1, 1).
				SetSelectable(true, false).
				SetSeparator(tview.Borders.Vertical)
			table.SetBackgroundColor(tcell.ColorDefault)
			table.SetTitle(" All Zones Availability ")
			table.SetTitleAlign(tview.AlignLeft)

			zones := scw.AllZones
			// Header row.
			table.SetCell(0, 0, tview.NewTableCell(" Name ").
				SetTextColor(accentColor).
				SetAttributes(tcell.AttrBold).
				SetSelectable(false))
			for i, z := range zones {
				table.SetCell(0, i+1, tview.NewTableCell(" "+z.String()+" ").
					SetTextColor(accentColor).
					SetAttributes(tcell.AttrBold).
					SetSelectable(false))
			}
			// Data rows.
			for r, row := range rows {
				name := row.Cols[0]
				table.SetCell(r+1, 0, tview.NewTableCell(" "+name+" ").
					SetTextColor(accentColor).
					SetSelectable(false))
				for i := range zones {
					idx := 5 + i
					val := "—"
					if idx < len(row.Cols) {
						val = row.Cols[idx]
					}
					table.SetCell(r+1, i+1, tview.NewTableCell(" "+val+" ").
						SetTextColor(availabilityColor(val)).
						SetAlign(tview.AlignCenter))
				}
			}

			closeFn := func() {
				a.view.SetRoot(a.layout, true).SetFocus(a.table)
			}
			table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
				switch event.Key() {
				case tcell.KeyEsc:
					closeFn()
					return nil
				case tcell.KeyRune:
					if event.Rune() == 'q' {
						closeFn()
						return nil
					}
				}
				return event
			})

			flex := tview.NewFlex().
				SetDirection(tview.FlexRow).
				AddItem(table, 0, 1, true).
				AddItem(a.status, 1, 0, false)
			a.view.SetRoot(flex, true).SetFocus(table)
			a.setStatus(accentBoldTag+"all zones availability[-] — %d SKUs × %d zones — [white]Esc[-] close", len(rows), len(zones))
		})
	}()
}
