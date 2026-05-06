package app

import (
	"net/http"
	"strings"

	"neutrino/internal/repo"
)

type nodesPageData struct {
	Nodes []nodeViewItem
}

type nodeViewItem struct {
	ID                  int64
	Name                string
	CoreType            string
	Protocol            string
	Host                string
	Port                int
	Enabled             bool
	Managed             bool
	AgentURL            string
	LastSeenAt          string
	LastError           string
	DesiredUsersVersion string
	AppliedUsersVersion string
	DesiredXrayVersion  string
	AppliedXrayVersion  string
}

type trafficPageData struct {
	Users []trafficFilterOption
	Nodes []trafficFilterOption
}

type trafficFilterOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type enforcementsPageData struct {
	Items []repo.EnforcementLog
}

func (a *App) handleNodesPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	nodes, err := a.nodes().List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	items := make([]nodeViewItem, 0, len(nodes))
	panelLoc := a.cfg.PanelLocation()
	for _, n := range nodes {
		item := nodeViewItem{
			ID:                  n.ID,
			Name:                n.Name,
			CoreType:            n.CoreType,
			Protocol:            n.Protocol,
			Host:                n.Host,
			Port:                n.Port,
			Enabled:             n.Enabled,
			Managed:             n.Managed,
			AgentURL:            n.AgentURL,
			DesiredUsersVersion: n.DesiredUsersVersion,
			AppliedUsersVersion: n.AppliedUsersVersion,
			DesiredXrayVersion:  n.DesiredXrayVersion,
			AppliedXrayVersion:  n.AppliedXrayVersion,
		}
		if strings.TrimSpace(item.Host) == "" && strings.TrimSpace(n.ObservedIP) != "" {
			item.Host = strings.TrimSpace(n.ObservedIP)
		}
		if n.LastSeenAt != nil {
			item.LastSeenAt = n.LastSeenAt.In(panelLoc).Format("2006-01-02 15:04:05")
		}
		item.LastError = n.LastError
		items = append(items, item)
	}

	current, _ := a.currentAdmin(r)
	data := PageData{
		CurrentAdmin: current,
		ActivePage:   "nodes",
		CSRFToken:    a.csrfTokenFor(r),
		Content: nodesPageData{
			Nodes: items,
		},
	}

	if err := a.pages["nodes"].ExecuteTemplate(w, "base.tmpl", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) handleTrafficPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	users, err := a.users().List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nodes, err := a.nodes().List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	userFilters := make([]trafficFilterOption, 0, len(users))
	for _, u := range users {
		userFilters = append(userFilters, trafficFilterOption{
			ID:   u.ID,
			Name: u.Username,
		})
	}
	nodeFilters := make([]trafficFilterOption, 0, len(nodes))
	for _, n := range nodes {
		nodeFilters = append(nodeFilters, trafficFilterOption{
			ID:   n.ID,
			Name: n.Name,
		})
	}

	current, _ := a.currentAdmin(r)
	data := PageData{
		CurrentAdmin: current,
		ActivePage:   "traffic",
		CSRFToken:    a.csrfTokenFor(r),
		Content: trafficPageData{
			Users: userFilters,
			Nodes: nodeFilters,
		},
	}

	if err := a.pages["traffic"].ExecuteTemplate(w, "base.tmpl", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) handleEnforcementsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	items, err := a.ops().ListEnforcementLogs(r.Context(), 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	current, _ := a.currentAdmin(r)
	data := PageData{CurrentAdmin: current, ActivePage: "enforcements", CSRFToken: a.csrfTokenFor(r), Content: enforcementsPageData{Items: items}}
	if err := a.pages["enforcements"].ExecuteTemplate(w, "base.tmpl", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
