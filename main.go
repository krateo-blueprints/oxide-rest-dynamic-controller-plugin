// oxide-rest-dynamic-controller-plugin is a Krateo rest-dynamic-controller
// "plugin" (wrapper web service) for the OxideInstance resource.
//
// The generic rest-dynamic-controller only speaks CRUD. Oxide, however, needs
// two extra actions to bring an instance up that a boot disk can actually boot
// from — attach the boot disk and start the instance — and those are not
// expressible as create/get/update/delete. Worse, Oxide's create-time
// `boot_disk` attach silently no-ops when the disk is not yet provisioned, so a
// declarative "apply disk + instance together" race leaves the instance stopped
// with a detached boot disk and never self-heals.
//
// This plugin fixes that by making GET an *idempotent reconcile*: on every
// observe it (re)attaches the boot disk recorded in `boot_disk_id` and, once
// attached, starts the instance. Because the controller GETs on every resync,
// the instance converges to running regardless of create ordering.
//
// The plugin forwards the caller's Authorization header to Oxide and targets
// the silo given by OXIDE_API_URL.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	base := strings.TrimRight(os.Getenv("OXIDE_API_URL"), "/")
	if base == "" {
		log.Fatal("OXIDE_API_URL is required")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	h := &handler{oxide: base, client: &http.Client{Timeout: 60 * time.Second}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/instances", h.create)
	mux.HandleFunc("GET /v1/instances/{instance}", h.get)
	mux.HandleFunc("PUT /v1/instances/{instance}", h.update)
	mux.HandleFunc("DELETE /v1/instances/{instance}", h.delete)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	log.Printf("oxide-instance plugin listening on %s -> %s", addr, base)
	log.Fatal(http.ListenAndServe(addr, mux))
}

type handler struct {
	oxide  string
	client *http.Client
}

// oxideReq performs an Oxide API call, forwarding the caller's bearer token.
func (h *handler) oxideReq(auth, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.oxide+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

func (h *handler) oxideJSON(auth, method, path string, body any) (map[string]any, int, error) {
	code, data, err := h.oxideReq(auth, method, path, body)
	if err != nil {
		return nil, code, err
	}
	var m map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &m)
	}
	return m, code, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, code int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

// create posts the instance to Oxide with start forced off, so we control the
// boot-disk attach and start ourselves via reconcile. boot_disk is passed
// through so Oxide records boot_disk_id (the reconcile source of truth).
func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	project := r.URL.Query().Get("project")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	// Force start=false: the instance is started by reconcile once the boot
	// disk is confirmed attached (a blank/not-ready disk cannot boot).
	body["start"] = false

	code, data, err := h.oxideReq(auth, http.MethodPost, "/v1/instances?project="+project, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if code >= 300 {
		writeRaw(w, code, data)
		return
	}
	// Best-effort immediate reconcile; the controller's next GET will finish it.
	name, _ := body["name"].(string)
	if inst, rc, _ := h.reconcile(auth, project, name); rc < 300 && inst != nil {
		writeJSON(w, code, inst)
		return
	}
	writeRaw(w, code, data)
}

// get is the reconcile: it converges the instance (attach boot disk, then start)
// and returns the current state.
func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	project := r.URL.Query().Get("project")
	name := r.PathValue("instance")
	inst, code, err := h.reconcile(auth, project, name)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if inst == nil {
		w.WriteHeader(code)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (h *handler) update(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	project := r.URL.Query().Get("project")
	name := r.PathValue("instance")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	code, data, err := h.oxideReq(auth, http.MethodPut, "/v1/instances/"+name+"?project="+project, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeRaw(w, code, data)
}

// delete stops a running instance before deleting it (Oxide refuses to delete a
// running instance).
func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	project := r.URL.Query().Get("project")
	name := r.PathValue("instance")

	inst, code, _ := h.oxideJSON(auth, http.MethodGet, "/v1/instances/"+name+"?project="+project, nil)
	if code == http.StatusNotFound {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if state, _ := inst["run_state"].(string); state == "running" || state == "starting" {
		_, _, _ = h.oxideReq(auth, http.MethodPost, "/v1/instances/"+name+"/stop?project="+project, nil)
		h.waitState(auth, project, name, "stopped", 30*time.Second)
	}
	rc, data, err := h.oxideReq(auth, http.MethodDelete, "/v1/instances/"+name+"?project="+project, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeRaw(w, rc, data)
}

// reconcile is the convergence loop for a single instance:
//  1. if the recorded boot disk is not attached, stop (if needed) and attach it;
//  2. once the boot disk is attached and the instance is stopped, start it.
//
// Each step is idempotent and best-effort: a step that cannot complete yet
// (e.g. the disk is still provisioning) simply leaves the instance for the next
// observe to retry.
func (h *handler) reconcile(auth, project, name string) (map[string]any, int, error) {
	inst, code, err := h.oxideJSON(auth, http.MethodGet, "/v1/instances/"+name+"?project="+project, nil)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	if code >= 300 {
		return nil, code, nil // typically 404 -> resource not found
	}

	bootDiskID, _ := inst["boot_disk_id"].(string)
	if bootDiskID != "" && !h.bootDiskAttached(auth, project, name, bootDiskID) {
		if state, _ := inst["run_state"].(string); state == "running" || state == "starting" {
			_, _, _ = h.oxideReq(auth, http.MethodPost, "/v1/instances/"+name+"/stop?project="+project, nil)
			h.waitState(auth, project, name, "stopped", 30*time.Second)
		}
		// attach by id; harmless if it fails because the disk isn't ready yet.
		_, _, _ = h.oxideReq(auth, http.MethodPost, "/v1/instances/"+name+"/disks/attach?project="+project,
			map[string]any{"disk": bootDiskID})
		inst, _, _ = h.oxideJSON(auth, http.MethodGet, "/v1/instances/"+name+"?project="+project, nil)
	}

	// Start only when the boot disk is genuinely attached and we're stopped.
	if bootDiskID != "" && h.bootDiskAttached(auth, project, name, bootDiskID) {
		if state, _ := inst["run_state"].(string); state == "stopped" {
			_, _, _ = h.oxideReq(auth, http.MethodPost, "/v1/instances/"+name+"/start?project="+project, nil)
			inst, _, _ = h.oxideJSON(auth, http.MethodGet, "/v1/instances/"+name+"?project="+project, nil)
		}
	}
	return inst, http.StatusOK, nil
}

func (h *handler) bootDiskAttached(auth, project, name, diskID string) bool {
	_, data, err := h.oxideReq(auth, http.MethodGet, "/v1/instances/"+name+"/disks?project="+project, nil)
	if err != nil {
		return false
	}
	var page struct {
		Items []struct {
			ID    string `json:"id"`
			State struct {
				State string `json:"state"`
			} `json:"state"`
		} `json:"items"`
	}
	_ = json.Unmarshal(data, &page)
	for _, d := range page.Items {
		if d.ID == diskID && d.State.State == "attached" {
			return true
		}
	}
	return false
}

func (h *handler) waitState(auth, project, name, want string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		inst, _, _ := h.oxideJSON(auth, http.MethodGet, "/v1/instances/"+name+"?project="+project, nil)
		if s, _ := inst["run_state"].(string); s == want {
			return
		}
		time.Sleep(2 * time.Second)
	}
}
