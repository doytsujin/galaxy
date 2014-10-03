package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/litl/galaxy/log"
	"github.com/litl/galaxy/shuttle/client"

	"github.com/gorilla/mux"
)

func getConfig(w http.ResponseWriter, r *http.Request) {
	w.Write(marshal(Registry.Config()))
}

func getStats(w http.ResponseWriter, r *http.Request) {
	if len(Registry.Config()) == 0 {
		w.WriteHeader(503)
	}
	w.Write(marshal(Registry.Stats()))
}

// misc debug info
func debugHandler(w http.ResponseWriter, r *http.Request) {
	ac := activeConns.List()
	js, err := json.MarshalIndent(&ac, "", "  ")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	js = append(js, '\n')
	w.Write(js)
}

func getService(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	serviceStats, err := Registry.ServiceStats(vars["service"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Write(marshal(serviceStats))
}

// Update a service and/or backends.
// Adding a `backends_only` query parameter will prevent the service from being
// shutdown and replaced if the ServiceConfig is not identical..
func postService(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorln(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	svcCfg := client.ServiceConfig{Name: vars["service"]}
	err = json.Unmarshal(body, &svcCfg)
	if err != nil {
		log.Errorln(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	invalidPorts := []string{
		listenAddr[strings.Index(listenAddr, ":")+1:],
		adminListenAddr[strings.Index(adminListenAddr, ":")+1:],
	}

	for _, port := range invalidPorts {
		if strings.HasSuffix(svcCfg.Addr, port) {
			log.Errorf("Cannot use shuttle port: %s for %s service listener. Shuttle is using it.", port, svcCfg.Name)
			http.Error(w, fmt.Sprintf("cannot use %s for listener port", port), http.StatusBadRequest)
			return
		}
	}

	// Add a new service, or update an existing one.
	if Registry.GetService(svcCfg.Name) == nil {
		if e := Registry.AddService(svcCfg); e != nil {
			log.Errorln(err)
			http.Error(w, e.Error(), http.StatusInternalServerError)
			return
		}
	} else if e := Registry.UpdateService(svcCfg); e != nil {
		log.Errorln("Unable to update service %s", svcCfg.Name)
		http.Error(w, e.Error(), http.StatusInternalServerError)
		return
	}

	go writeStateConfig()
	w.Write(marshal(Registry.Config()))
}

func deleteService(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	err := Registry.RemoveService(vars["service"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	go writeStateConfig()
	w.Write(marshal(Registry.Config()))
}

func getBackend(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	serviceName := vars["service"]
	backendName := vars["backend"]

	backend, err := Registry.BackendStats(serviceName, backendName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Write(marshal(backend))
}

func postBackend(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorln(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	backendName := vars["backend"]
	serviceName := vars["service"]

	backendCfg := client.BackendConfig{Name: backendName}
	err = json.Unmarshal(body, &backendCfg)
	if err != nil {
		log.Errorln(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := Registry.AddBackend(serviceName, backendCfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	go writeStateConfig()
	w.Write(marshal(Registry.Config()))
}

func deleteBackend(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	serviceName := vars["service"]
	backendName := vars["backend"]

	if err := Registry.RemoveBackend(serviceName, backendName); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	go writeStateConfig()
	w.Write(marshal(Registry.Config()))
}

func addHandlers() {
	r := mux.NewRouter()
	r.HandleFunc("/", getStats).Methods("GET")
	r.HandleFunc("/_config", getConfig).Methods("GET")
	r.HandleFunc("/_debug", debugHandler).Methods("GET")
	r.HandleFunc("/{service}", getService).Methods("GET")
	r.HandleFunc("/{service}", postService).Methods("PUT", "POST")
	r.HandleFunc("/{service}", deleteService).Methods("DELETE")
	r.HandleFunc("/{service}/{backend}", getBackend).Methods("GET")
	r.HandleFunc("/{service}/{backend}", postBackend).Methods("PUT", "POST")
	r.HandleFunc("/{service}/{backend}", deleteBackend).Methods("DELETE")
	http.Handle("/", r)
}

func startAdminHTTPServer() {
	defer wg.Done()
	addHandlers()
	log.Println("Admin server listening on", adminListenAddr)

	netw := "tcp"

	if strings.HasPrefix(adminListenAddr, "/") {
		netw = "unix"

		// remove our old socket if we left it lying around
		if stats, err := os.Stat(adminListenAddr); err == nil {
			if stats.Mode()&os.ModeSocket != 0 {
				os.Remove(adminListenAddr)
			}
		}

		defer os.Remove(adminListenAddr)
	}

	listener, err := net.Listen(netw, adminListenAddr)
	if err != nil {
		log.Fatalln(err)
	}

	http.Serve(listener, nil)
}
