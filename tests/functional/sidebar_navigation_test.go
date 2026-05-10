package functional_test

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

type sidebarPageExpectation struct {
	page  string
	path  string
	title string
}

func TestFunctional_SidebarNavigationUsesStableHTMXMainTarget(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.request(t, http.MethodGet, "/users", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	body := decodeBodyText(t, resp)
	doc := parseHTMLDoc(t, body)

	if got := len(findElements(doc, func(n *html.Node) bool {
		v, ok := attr(n, "id")
		return ok && v == "app-main"
	})); got != 1 {
		t.Fatalf("expected exactly one #app-main, got %d", got)
	}

	expected := []sidebarPageExpectation{
		{page: "users", path: "/users"},
		{page: "nodes", path: "/nodes"},
		{page: "traffic", path: "/traffic"},
		{page: "enforcements", path: "/enforcements"},
		{page: "ops", path: "/ops"},
	}
	links := sidebarLinksByPage(t, doc)
	if got, want := len(links), len(expected); got != want {
		t.Fatalf("expected %d sidebar links, got %d", want, got)
	}
	for _, item := range expected {
		link := links[item.page]
		if link == nil {
			t.Fatalf("missing sidebar link for page %q", item.page)
		}
		assertAttr(t, link, "href", item.path)
		assertAttr(t, link, "hx-get", item.path)
		assertAttr(t, link, "hx-target", "#app-main")
		assertAttr(t, link, "hx-select", "#app-main")
		assertAttr(t, link, "hx-swap", "outerHTML show:window:top")
		assertAttr(t, link, "hx-push-url", "true")
		if hasAncestorWithID(link, "app-main") {
			t.Fatalf("sidebar link %q must live outside #app-main so partial swaps do not replace the sidebar", item.page)
		}
	}
}

func TestFunctional_SidebarToggleRendersOnlyInMainHeader(t *testing.T) {
	env := setupTestEnv(t)
	resp := env.request(t, http.MethodGet, "/users", nil, true, "")
	mustStatus(t, resp, http.StatusOK)
	body := decodeBodyText(t, resp)
	doc := parseHTMLDoc(t, body)

	toggles := findElements(doc, func(n *html.Node) bool {
		v, ok := attr(n, "aria-label")
		return ok && v == "切换侧边栏"
	})
	if got := len(toggles); got != 1 {
		t.Fatalf("expected exactly one sidebar toggle, got %d", got)
	}
	if hasAncestorWithID(toggles[0], "sidebar") {
		t.Fatalf("sidebar toggle should be rendered in the main header, not inside #sidebar")
	}
	if !hasAncestorWithID(toggles[0], "app-main") {
		t.Fatalf("sidebar toggle should be rendered inside #app-main")
	}
}

func TestFunctional_SidebarActiveStateMatchesRenderedRoute(t *testing.T) {
	env := setupTestEnv(t)
	pages := []sidebarPageExpectation{
		{page: "users", path: "/users", title: "用户管理"},
		{page: "nodes", path: "/nodes", title: "节点管理"},
		{page: "traffic", path: "/traffic", title: "流量分析"},
		{page: "enforcements", path: "/enforcements", title: "执行记录"},
		{page: "ops", path: "/ops", title: "运维监控"},
	}

	for _, tc := range pages {
		t.Run(tc.page, func(t *testing.T) {
			resp := env.request(t, http.MethodGet, tc.path, nil, true, "")
			mustStatus(t, resp, http.StatusOK)
			body := decodeBodyText(t, resp)
			if !strings.Contains(body, tc.title) {
				t.Fatalf("expected page body to contain title %q", tc.title)
			}
			doc := parseHTMLDoc(t, body)
			links := sidebarLinksByPage(t, doc)
			if got, want := len(links), len(pages); got != want {
				t.Fatalf("expected %d sidebar links, got %d", want, got)
			}
			for _, item := range pages {
				link := links[item.page]
				if link == nil {
					t.Fatalf("missing sidebar link for page %q", item.page)
				}
				ariaCurrent, hasAriaCurrent := attr(link, "aria-current")
				if item.page == tc.page {
					if !hasAriaCurrent || ariaCurrent != "page" {
						t.Fatalf("active page %q should have aria-current=page, got %q ok=%v", item.page, ariaCurrent, hasAriaCurrent)
					}
					for _, className := range []string{"bg-sidebar-active", "text-white", "font-medium"} {
						if !hasClass(link, className) {
							t.Fatalf("active page %q missing class %q: %s", item.page, className, mustAttr(t, link, "class"))
						}
					}
					continue
				}
				if hasAriaCurrent {
					t.Fatalf("inactive page %q unexpectedly has aria-current=%q", item.page, ariaCurrent)
				}
				for _, className := range []string{"text-slate-300", "hover:text-white", "hover:bg-sidebar-hover"} {
					if !hasClass(link, className) {
						t.Fatalf("inactive page %q missing class %q: %s", item.page, className, mustAttr(t, link, "class"))
					}
				}
			}
		})
	}
}

func parseHTMLDoc(t *testing.T, body string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse html: %v", err)
	}
	return doc
}

func sidebarLinksByPage(t *testing.T, doc *html.Node) map[string]*html.Node {
	t.Helper()
	out := map[string]*html.Node{}
	for _, link := range findElements(doc, func(n *html.Node) bool {
		_, ok := attr(n, "data-sidebar-page")
		return ok
	}) {
		page := mustAttr(t, link, "data-sidebar-page")
		if previous := out[page]; previous != nil {
			t.Fatalf("duplicate sidebar link for page %q", page)
		}
		out[page] = link
	}
	return out
}

func findElements(root *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode && pred(n) {
			out = append(out, n)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return out
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func mustAttr(t *testing.T, n *html.Node, key string) string {
	t.Helper()
	v, ok := attr(n, key)
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	return v
}

func assertAttr(t *testing.T, n *html.Node, key, want string) {
	t.Helper()
	got := mustAttr(t, n, key)
	if got != want {
		t.Fatalf("attribute %q got %q, want %q", key, got, want)
	}
}

func hasClass(n *html.Node, className string) bool {
	classAttr, ok := attr(n, "class")
	if !ok {
		return false
	}
	for _, token := range strings.Fields(classAttr) {
		if token == className {
			return true
		}
	}
	return false
}

func hasAncestorWithID(n *html.Node, id string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if got, ok := attr(p, "id"); ok && got == id {
			return true
		}
	}
	return false
}
