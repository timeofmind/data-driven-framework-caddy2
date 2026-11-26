package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/timeofmind/data-driven-framework-go/listener/media"
	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/ux"
)

// The admin area every app has: the server's own sections — Service
// Configuration (services.go) and Media (here) — and the app's, named in
// admin.yaml beside app.yaml (or, without one, its pages under /admin/).
// The theme styles it like the rest of the app; the sections are the same
// in every app, and a builder adds to them with the YAML file.
//
// Media is the app's media library (the hosting's dd-media: files its
// admins publish, served from the hosting's media host). The page's calls
// go to this server (/_dd/media/…), which checks the admin session and
// passes them to dd-media with the node's token (DD_MEDIA_TOKEN,
// DD_MEDIA_URL in the node's environment, given by the hosting); a file is
// streamed through to the slot dd-media signed, never kept here.

const (
	mediaPath    = "/admin/media"
	mediaAPIPath = "/_dd/media"
)

// AdminSection is one section of the admin area, from admin.yaml. Level,
// when given, keeps the section to admins whose authority level is lower
// than it (a lower number is more authority; owners are 0). An entry at
// the path of one of the server's own sections sets that section's level.
type AdminSection struct {
	Label string `yaml:"label"`
	Path  string `yaml:"path"`
	Level int    `yaml:"level"`
}

type adminFile struct {
	Sections []AdminSection `yaml:"sections"`
}

// loadAdminSections reads <dir>/admin.yaml: the app's own sections, in
// order, after the server's (Services, Media, Admins — the Admins page at
// adminsAt). Without the file, the app's pages whose path is under /admin/
// are its sections, in page order, each with its page's level.
func loadAdminSections(dir string, pages *ux.Site, adminsAt, signinAt, protectAt, dnsAt, checkoutAt string) ([]ux.Anchor, error) {
	out := []ux.Anchor{{Label: "Services", URL: servicesPath, Level: servicesLevel}}
	if checkoutAt != "" { // the routing names a payment listener
		out = append(out, ux.Anchor{Label: "Payment Checkout", URL: checkoutAt, Level: servicesLevel})
	}
	out = append(out, ux.Anchor{Label: "Media", URL: mediaPath}, ux.Anchor{Label: "Admins", URL: adminsAt, Level: adminsLevel})
	if signinAt != "" { // the routing names a signin listener
		out = append(out, ux.Anchor{Label: "Sign-in", URL: signinAt, Level: servicesLevel}) // keys, like Services
	}
	if protectAt != "" { // the app is behind an edge with the bot defence
		out = append(out, ux.Anchor{Label: "Bot Protection", URL: protectAt, Level: servicesLevel})
	}
	if dnsAt != "" { // the routing names a zone the server keeps
		out = append(out, ux.Anchor{Label: "DNS", URL: dnsAt, Level: servicesLevel})
	}
	own := len(out)
	b, err := os.ReadFile(filepath.Join(dir, "admin.yaml"))
	switch {
	case err == nil:
		var f adminFile
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("admin.yaml: %w", err)
		}
		for i, s := range f.Sections {
			if s.Level < 0 || s.Level > maxAdminLevel+1 {
				return nil, fmt.Errorf("admin.yaml: section %d: level is 1 to %d (0 or absent: any admin)", i+1, maxAdminLevel+1)
			}
			server := false
			for k := 0; k < own; k++ {
				if out[k].URL == strings.TrimRight(s.Path, "/") {
					out[k].Level, server = s.Level, true // the server's own: its level alone is the app's to say
				}
			}
			if server {
				continue
			}
			if s.Label == "" || !strings.HasPrefix(s.Path, "/") {
				return nil, fmt.Errorf("admin.yaml: section %d needs a label and a path starting with /", i+1)
			}
			out = append(out, ux.Anchor{Label: s.Label, URL: strings.TrimRight(s.Path, "/"), Level: s.Level})
		}
	case errors.Is(err, os.ErrNotExist):
		if pages != nil {
			for _, p := range pages.Pages {
				if !strings.HasPrefix(p.Path, "/admin/") || strings.Contains(p.Path, "{") || p.Shared {
					continue
				}
				if pth := strings.TrimRight(p.Path, "/"); pth == adminsPath || pth == signinPath || pth == protectPath || pth == dnsPath || pth == checkoutPath {
					continue // the app's own page there is reached by its own links
				}
				out = append(out, ux.Anchor{Label: p.LabelText(), URL: strings.TrimRight(p.Path, "/"), Level: p.Level})
			}
		}
	default:
		return nil, err
	}
	return out, nil
}

// mediaClient is dd-media as this node reaches it; nil when the node has
// no media token (an app hosted elsewhere).
func (a *App) mediaClient() *media.Client {
	if a.cfg.MediaToken == "" {
		return nil
	}
	url := a.cfg.MediaURL
	if url == "" {
		url = "http://127.0.0.1:8095"
	}
	return &media.Client{URL: url, Token: a.cfg.MediaToken, HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

type mediaPage struct {
	Available bool
	Problem   string
}

// mediaAdmin serves the Media page: admins only, with the authority the
// section asks for.
func (a *App) mediaAdmin(rq *request) error {
	if ok, err := a.adminSection(rq, mediaPath); !ok {
		return err
	}
	pg := mediaPage{Available: a.mediaClient().Available()}
	if !pg.Available {
		pg.Problem = "The media library is not available on this server: it has no media token (DD_MEDIA_TOKEN). On the hosting, every node is given one; an app run elsewhere keeps its files in its own theme or an s3 listener's bucket."
	}
	// Thumbnails come from the media host: this page alone lets images in
	// from anywhere over https.
	rq.w.Header().Set("Content-Security-Policy", strings.Replace(a.csp(), "img-src 'self' data:", "img-src 'self' data: https:", 1))
	a.site.Render(a.webRequest(rq), "media", "Media", pg)
	return nil
}

// mediaAPI serves /_dd/media/…: the page's calls, passed to dd-media with
// the node's token. Admins only, with the CSRF token as the JSON API takes
// it (X-CSRF-Token).
//
//	POST   /_dd/media/slot            {filename, content_type, size, alt, login} → the slot (media_id, url, upload_token)
//	POST   /_dd/media/{id}/upload     the file as the body; X-Media-Slot: the slot's upload_token
//	POST   /_dd/media/{id}/complete
//	GET    /_dd/media?type=&after=
//	GET    /_dd/media/{id}
//	PATCH  /_dd/media/{id}            {alt, title}
//	DELETE /_dd/media/{id}
func (a *App) mediaAPI(rq *request, parts []string) {
	if len(parts) == 1 && parts[0] == "" {
		parts = nil
	}
	a.resolveSession(rq)
	a.ensureCSRF(rq)
	rq.w.Header().Set("Cache-Control", "no-store")
	if err := a.mediaRoute(rq, parts); err != nil {
		a.apiError(rq, err)
	}
}

func (a *App) mediaRoute(rq *request, parts []string) error {
	if rq.p == nil {
		return errStatus(http.StatusUnauthorized, "sign in first")
	}
	m := rq.r.Method
	if !rq.p.AdminAllowed(a.sectionLevel(mediaPath)) {
		if !rq.p.SignedIn() {
			return errForbidden
		}
		if !a.checkCSRF(rq, rq.r.Header.Get("X-CSRF-Token")) {
			return errStatus(http.StatusForbidden, "missing or invalid X-CSRF-Token header")
		}
		return a.memberPicture(rq, parts)
	}
	if m != http.MethodGet && !a.checkCSRF(rq, rq.r.Header.Get("X-CSRF-Token")) {
		return errStatus(http.StatusForbidden, "missing or invalid X-CSRF-Token header")
	}
	c := a.mediaClient()
	if !c.Available() {
		return errStatus(http.StatusServiceUnavailable, "the media library is not available on this server (no media token)")
	}
	switch {
	case len(parts) == 0 && m == http.MethodGet:
		q := ""
		if rq.r.URL.RawQuery != "" {
			q = "?" + rq.r.URL.RawQuery
		}
		return a.relay(rq, c, http.MethodGet, "/v1/media"+q, nil)
	case len(parts) == 1 && parts[0] == "slot" && m == http.MethodPost:
		return a.mediaSlot(rq, c)
	case len(parts) == 2 && parts[1] == "upload" && m == http.MethodPost:
		return a.mediaUpload(rq, c, parts[0])
	case len(parts) == 2 && parts[1] == "complete" && m == http.MethodPost:
		return a.relay(rq, c, http.MethodPost, "/v1/media/"+parts[0]+"/complete", nil)
	case len(parts) == 1 && m == http.MethodGet:
		return a.relay(rq, c, http.MethodGet, "/v1/media/"+parts[0], nil)
	case len(parts) == 1 && m == http.MethodPatch:
		body, err := io.ReadAll(io.LimitReader(rq.r.Body, 64<<10))
		if err != nil {
			return err
		}
		return a.relay(rq, c, http.MethodPatch, "/v1/media/"+parts[0], body)
	case len(parts) == 1 && m == http.MethodDelete:
		return a.relay(rq, c, http.MethodDelete, "/v1/media/"+parts[0], nil)
	}
	return errStatus(http.StatusNotFound, "no such media call")
}

// relay passes a call to dd-media and its answer back as it came.
func (a *App) relay(rq *request, c *media.Client, method, path string, body []byte) error {
	status, rb, err := c.Do(rq.ctx(), method, path, body)
	if err != nil {
		a.log.Warn("dd-media did not answer", "error", err)
		return errStatus(http.StatusBadGateway, "the media library did not answer: "+err.Error())
	}
	rq.w.Header().Set("Content-Type", "application/json")
	rq.w.WriteHeader(status)
	rq.w.Write(rb)
	return nil
}

// mediaSlot asks dd-media for a slot and answers with the record and an
// upload token: the slot's presigned POST, sealed so the upload may reach
// any node of the cluster and still be sent on. The login is the admin's.
func (a *App) mediaSlot(rq *request, c *media.Client) error {
	var req struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
		Alt         string `json:"alt"`
		Login       string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(rq.r.Body, 64<<10)).Decode(&req); err != nil {
		return errStatus(http.StatusBadRequest, "the body is not the JSON of a slot request")
	}
	if req.Login == "" || !rq.p.AdminAllowed(a.sectionLevel(mediaPath)) {
		req.Login = rq.p.Name
	}
	if !rq.p.AdminAllowed(a.sectionLevel(mediaPath)) { // a member's picture, for formatted text
		req.Login = fmt.Sprintf("member %d: %s", rq.p.ID, rq.p.Name)
		if !memberPictureTypes[strings.ToLower(req.ContentType)] {
			return errStatus(http.StatusUnsupportedMediaType, "only a picture can be added here (JPEG, PNG, WebP or GIF)")
		}
		if req.Size <= 0 || req.Size > memberPictureMax {
			return errStatus(http.StatusRequestEntityTooLarge, fmt.Sprintf("a picture can be at most %d MB", memberPictureMax>>20))
		}
		if !a.memberPictures.take(rq.p.ID, time.Now()) {
			return errStatus(http.StatusTooManyRequests, fmt.Sprintf("you have added %d pictures today: try again tomorrow, or ask the club", memberPicturesPerDay))
		}
	}
	b, _ := json.Marshal(req)
	status, rb, err := c.Do(rq.ctx(), http.MethodPost, "/v1/uploads", b)
	if err != nil {
		return errStatus(http.StatusBadGateway, "the media library did not answer: "+err.Error())
	}
	if status < 200 || status > 299 {
		rq.w.Header().Set("Content-Type", "application/json")
		rq.w.WriteHeader(status)
		rq.w.Write(rb)
		return nil
	}
	var slot struct {
		MediaID string          `json:"media_id"`
		Status  string          `json:"status"`
		URL     string          `json:"url"`
		Upload  json.RawMessage `json:"upload"`
	}
	if err := json.Unmarshal(rb, &slot); err != nil || slot.MediaID == "" {
		return errStatus(http.StatusBadGateway, "the media library's slot is not as expected")
	}
	tok, err := seal(a.cfg.TokenKey, string(slot.Upload))
	if err != nil {
		// no token key: the upload token is the slot itself, readable but
		// useless to anyone else (it only lets the file be sent to its slot)
		tok = "plain:" + base64.RawURLEncoding.EncodeToString(slot.Upload)
	}
	return writeJSON(rq, http.StatusCreated, map[string]any{"media_id": slot.MediaID, "status": slot.Status, "url": slot.URL,
		"content_type": req.ContentType, "size": req.Size, "upload_token": tok})
}

// uploadSlot is a slot's presigned POST, from its upload token.
type uploadSlot struct {
	URL    string            `json:"url"`
	Fields map[string]string `json:"fields"`
}

// slotOf reads the request's upload token (X-Media-Slot): the slot of the
// file mediaID, or an error.
func (a *App) slotOf(rq *request, mediaID string) (*uploadSlot, error) {
	tok := rq.r.Header.Get("X-Media-Slot")
	if tok == "" {
		return nil, errStatus(http.StatusBadRequest, "X-Media-Slot (the slot's upload_token) is required")
	}
	var raw string
	if strings.HasPrefix(tok, "plain:") {
		if !rq.p.AdminAllowed(a.sectionLevel(mediaPath)) {
			return nil, errStatus(http.StatusBadRequest, "the upload token is not this server's") // a member's is always sealed
		}
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, "plain:"))
		if err != nil {
			return nil, errStatus(http.StatusBadRequest, "the upload token is not readable")
		}
		raw = string(b)
	} else {
		var err error
		if raw, err = unseal(a.cfg.TokenKey, tok); err != nil {
			return nil, errStatus(http.StatusBadRequest, "the upload token is not this server's")
		}
	}
	var up uploadSlot
	if err := json.Unmarshal([]byte(raw), &up); err != nil || up.URL == "" {
		return nil, errStatus(http.StatusBadRequest, "the upload token names no slot")
	}
	if key := up.Fields["key"]; mediaID == "" || !strings.HasSuffix(key, mediaID) {
		return nil, errStatus(http.StatusBadRequest, "the upload token is another file's")
	}
	return &up, nil
}

// mediaUpload streams the request's body — the file — to the slot named by
// the upload token, as the slot's presigned POST.
func (a *App) mediaUpload(rq *request, c *media.Client, mediaID string) error {
	up, err := a.slotOf(rq, mediaID)
	if err != nil {
		return err
	}
	ctype := rq.r.Header.Get("Content-Type")
	if ct := up.Fields["Content-Type"]; ct != "" {
		ctype = ct // what the slot was signed for
	}
	size := rq.r.ContentLength
	if size <= 0 {
		return errStatus(http.StatusLengthRequired, "the file's size (Content-Length) is required")
	}
	name := rq.r.URL.Query().Get("name")
	if name == "" {
		name = "file"
	}
	ctx, cancel := context.WithTimeout(rq.ctx(), 10*time.Minute)
	defer cancel()
	if err := c.UploadStream(ctx, up.URL, up.Fields, name, ctype, size, rq.r.Body); err != nil {
		var ae *media.APIError
		if errors.As(err, &ae) {
			return errStatus(http.StatusBadGateway, fmt.Sprintf("the bucket refused the file (HTTP %d): %s", ae.Status, ae.Message))
		}
		return errStatus(http.StatusBadGateway, "the file did not reach the bucket: "+err.Error())
	}
	return writeJSON(rq, http.StatusOK, map[string]any{"media_id": mediaID, "uploaded": true})
}

// A signed-in member (not an admin) may add a picture to formatted text (the
// editor of a format: rich slot): a slot for one picture (JPEG, PNG, WebP or
// GIF, up to memberPictureMax), its upload and its completion — the last
// two only with the slot's upload token, so only the member's own picture.
// The library's other calls (listing, changing, deleting) stay the admins'.
// A member adds at most memberPicturesPerDay pictures a day on each node.
const (
	memberPictureMax     = 8 << 20
	memberPicturesPerDay = 40
)

var memberPictureTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true}

// MemberImages says whether a page's editor may upload the viewer's
// pictures: a signed-in member, on a node with the media library.
func (a *App) memberImages(p *runtime.Principal) bool {
	return p.SignedIn() && len(a.cfg.TokenKey) > 0 && a.mediaClient().Available()
}

func (a *App) memberPicture(rq *request, parts []string) error {
	c := a.mediaClient()
	if !a.memberImages(rq.p) {
		return errStatus(http.StatusServiceUnavailable, "pictures can't be uploaded on this server (no media library)")
	}
	if rq.r.Method != http.MethodPost {
		return errForbidden
	}
	switch {
	case len(parts) == 1 && parts[0] == "slot":
		return a.mediaSlot(rq, c)
	case len(parts) == 2 && parts[1] == "upload":
		return a.mediaUpload(rq, c, parts[0])
	case len(parts) == 2 && parts[1] == "complete":
		if _, err := a.slotOf(rq, parts[0]); err != nil {
			return err
		}
		return a.relay(rq, c, http.MethodPost, "/v1/media/"+parts[0]+"/complete", nil)
	}
	return errForbidden
}

// pictureQuota counts each member's pictures today.
type pictureQuota struct {
	mu   sync.Mutex
	day  string
	used map[int64]int
}

func (q *pictureQuota) take(member int64, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if d := now.UTC().Format("2006-01-02"); d != q.day || q.used == nil {
		q.day, q.used = d, map[int64]int{}
	}
	if q.used[member] >= memberPicturesPerDay {
		return false
	}
	q.used[member]++
	return true
}

// sortedAnchors is for tests: the sections by label.
func sortedAnchors(in []ux.Anchor) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.Label+" "+a.URL)
	}
	sort.Strings(out)
	return out
}
