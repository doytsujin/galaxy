package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/garyburd/redigo/redis"
	"github.com/litl/galaxy/utils"
)

/*
All config opbects in redis will be stored in a hash with an id key.
Services will have id, version and environment keys; while Hosts will have id
and location keys.

TODO: IMPORTANT: make an atomic compare-and-swap script to save configs, or
      switch to ORDERED SETS and log changes
*/

type ServiceConfig struct {
	ID      int64
	Name    string
	Version string
	Env     map[string]string
}

type ServiceRegistration struct {
	// ID is used for ordering and conflict resolution.
	// Usualy set to time.Now().UnixNano()
	ID           int64     `json:"-"`
	ExternalIP   string    `json:"EXTERNAL_IP"`
	ExternalPort string    `json:"EXTERNAL_PORT"`
	InternalIP   string    `json:"INTERNAL_IP"`
	InternalPort string    `json:"INTERNAL_PORT"`
	ContainerID  string    `json:"CONTAINER_ID"`
	StartedAt    time.Time `json:"STARTED_AT"`
	Expires      time.Time `json:"-"`
	Path         string    `json:"-"`
}

type ServiceRegistry struct {
	redisPool    redis.Pool
	Env          string
	Pool         string
	HostIP       string
	Hostname     string
	TTL          uint64
	HostSSHAddr  string
	OutputBuffer *utils.OutputBuffer
}

type ConfigChange struct {
	ServiceConfig *ServiceConfig
	Error         error
}

func (s *ServiceRegistration) Equals(other ServiceRegistration) bool {
	return s.ExternalIP == other.ExternalIP &&
		s.ExternalPort == other.ExternalPort &&
		s.InternalIP == other.InternalIP &&
		s.InternalPort == other.InternalPort
}

func (r *ServiceRegistry) ensureHostname() string {
	if r.Hostname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			panic(err)
		}
		r.Hostname = hostname

	}
	return r.Hostname
}

// Build the Redis Pool
func (r *ServiceRegistry) Connect(redisHost string) {
	rwTimeout := 5 * time.Second

	redisPool := redis.Pool{
		MaxIdle:     1,
		IdleTimeout: 120 * time.Second,
		Dial: func() (redis.Conn, error) {
			return redis.DialTimeout("tcp", redisHost, rwTimeout, rwTimeout, rwTimeout)
		},
		// test every connection for now
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
	}

	r.redisPool = redisPool
}

func NewServiceConfig(app, version string, cfg map[string]string) (*ServiceConfig, error) {
	svcCfg := &ServiceConfig{
		ID:      time.Now().UnixNano(),
		Name:    app,
		Version: version,
		Env:     cfg,
	}

	return svcCfg, nil
}

func (r *ServiceRegistry) newServiceRegistration(container *docker.Container) *ServiceRegistration {
	//FIXME: We're using the first found port and assuming it's tcp.
	//How should we handle a service that exposes multiple ports
	//as well as tcp vs udp ports.
	var externalPort, internalPort string
	for k, _ := range container.NetworkSettings.Ports {
		externalPort = k.Port()
		internalPort = externalPort
		break
	}

	serviceRegistration := ServiceRegistration{
		ID:           time.Now().UnixNano(),
		ExternalIP:   r.HostIP,
		ExternalPort: externalPort,
		InternalIP:   container.NetworkSettings.IPAddress,
		InternalPort: internalPort,
		ContainerID:  container.ID,
		StartedAt:    container.Created,
	}
	return &serviceRegistration
}

func (r *ServiceRegistry) ServiceConfig(app string) (*ServiceConfig, error) {
	conn := r.redisPool.Get()
	defer conn.Close()

	matches, err := redis.Values(conn.Do("HGETALL", path.Join(r.Env, r.Pool, app)))
	if err != nil {
		fmt.Printf("ERROR: could not get ServiceConfig - %s\n", err)
	}

	svcCfg := &ServiceConfig{}
	err = redis.ScanStruct(matches, svcCfg)
	if err != nil {
		return nil, err
	}
	return svcCfg, nil
}

func (r *ServiceRegistry) SetServiceConfig(svcCfg *ServiceConfig) error {
	conn := r.redisPool.Get()
	defer conn.Close()

	env, err := json.Marshal(svcCfg.Env)
	if err != nil {
		return err
	}

	_, err = conn.Do("HMSET", path.Join(r.Env, r.Pool, svcCfg.Name), "id", svcCfg.ID,
		"version", svcCfg.Version, "environment", env)
	if err != nil {
		return err
	}
	return nil
}

func (r *ServiceRegistry) RegisterService(container *docker.Container, serviceConfig *ServiceConfig) error {
	registrationPath := path.Join(r.Env, r.Pool, "hosts", r.ensureHostname(), serviceConfig.Name)

	var existingRegistration ServiceRegistration

	panic("get host registration JSON")
	existingJson := []byte{}
	err := json.Unmarshal([]byte(existingJson), &existingRegistration)
	if err != nil {
		return err
	}

	if existingRegistration.StartedAt.After(container.Created) {
		return nil
	}

	serviceRegistration := r.newServiceRegistration(container)
	if serviceRegistration.Equals(existingRegistration) {
		statusLine := strings.Join([]string{
			container.ID[0:12],
			registrationPath,
			container.Config.Image,
			serviceRegistration.ExternalIP + ":" + serviceRegistration.ExternalPort,
			serviceRegistration.InternalIP + ":" + serviceRegistration.InternalPort,
			utils.HumanDuration(time.Now().Sub(container.Created)) + " ago",
			"In " + "some-amount-of-time", // FIXME: what is this?
		}, " | ")

		r.OutputBuffer.Log(statusLine)
		return nil
	}

	jsonReg, err := json.Marshal(serviceRegistration)
	if err != nil {
		return err
	}

	conn := r.redisPool.Get()
	defer conn.Close()

	// TODO: use a compare-and-swap SCRIPT
	_, err = conn.Do("HMSET", registrationPath, "id", serviceRegistration.ID, "location", jsonReg)
	if err != nil {
		return err
	}

	// TODO: SET TTL

	statusLine := strings.Join([]string{
		container.ID[0:12],
		registrationPath,
		container.Config.Image,
		serviceRegistration.ExternalIP + ":" + serviceRegistration.ExternalPort,
		serviceRegistration.InternalIP + ":" + serviceRegistration.InternalPort,
		utils.HumanDuration(time.Now().Sub(container.Created)) + " ago",
		"In " + "some-amount-of-time", // FIXME: what is this?
	}, " | ")

	r.OutputBuffer.Log(statusLine)

	return nil
}

func (r *ServiceRegistry) UnRegisterService(container *docker.Container, serviceConfig *ServiceConfig) error {

	registrationPath := path.Join(r.Env, r.Pool, "hosts", r.ensureHostname(), serviceConfig.Name)

	conn := r.redisPool.Get()
	defer conn.Close()

	_, err := conn.Do("DELETE", registrationPath)
	if err != nil {
		return err
	}

	statusLine := strings.Join([]string{
		container.ID[0:12],
		"",
		container.Config.Image,
		"",
		"",
		utils.HumanDuration(time.Now().Sub(container.Created)) + " ago",
		"",
	}, " | ")

	r.OutputBuffer.Log(statusLine)

	return nil
}

// TODO: IsRegistered shoud return bool, or be changed to GetServiceRegistration
func (r *ServiceRegistry) IsRegistered(container *docker.Container, serviceConfig *ServiceConfig) (*ServiceRegistration, error) {

	desiredServiceRegistration := r.newServiceRegistration(container)
	regPath := path.Join(r.Env, r.Pool, "hosts", r.ensureHostname(), serviceConfig.Name)

	var existingRegistration ServiceRegistration

	conn := r.redisPool.Get()
	defer conn.Close()

	location, err := redis.Bytes(conn.Do("HGET", regPath, "location"))
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(location, &existingRegistration)
	if err != nil {
		return nil, err
	}

	if existingRegistration.Equals(*desiredServiceRegistration) {
		return &existingRegistration, nil
	}

	return nil, fmt.Errorf("NOT FOUND")
}

// We need an ID to start from, so we know when something has changed.
// Return nil,nil if mothing has changed (for now)
func (r *ServiceRegistry) Watch(lastID int64, changes chan *ConfigChange, stop chan struct{}) {
	watchPath := path.Join(r.Env, r.Pool, "*")

	go func() {
		// TODO: default polling interval
		ticker := time.NewTicker(2 * time.Second)
		select {
		case <-stop:
			ticker.Stop()
			return
		case <-ticker.C:
			panic("KEYS " + watchPath)
			// GET CONFIGS AND CHECK IDs
			changes <- nil
		}
	}()
}

// TODO: log or return error?
func (r *ServiceRegistry) CountInstances(app string) int {
	conn := r.redisPool.Get()
	defer conn.Close()

	// TODO: convert to SCAN
	// TODO: Should this just sum hosts? (this counts all services on all hosts)
	matches, err := redis.Values(conn.Do("KEYS", path.Join(r.Env, r.Pool, "hosts", "*", app)))
	if err != nil {
		fmt.Printf("ERROR: could not count instances - %s\n", err)
	}

	return len(matches)
}

func (r *ServiceRegistry) EnvExists() (bool, error) {
	conn := r.redisPool.Get()
	defer conn.Close()

	// TODO: convert to SCAN
	matches, err := redis.Values(conn.Do("KEYS", path.Join(r.Env, "*")))
	if err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

func (r *ServiceRegistry) PoolExists() (bool, error) {
	conn := r.redisPool.Get()
	defer conn.Close()

	// TODO: convert to SCAN
	matches, err := redis.Values(conn.Do("KEYS", path.Join(r.Env, r.Pool, "*")))
	if err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

func (r *ServiceRegistry) AppExists(app string) (bool, error) {
	conn := r.redisPool.Get()
	defer conn.Close()

	// TODO: convert to SCAN
	matches, err := redis.Values(conn.Do("KEYS", path.Join(r.Env, r.Pool, app)))
	if err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

func (r *ServiceRegistry) ListApps() ([]ServiceConfig, error) {
	conn := r.redisPool.Get()
	defer conn.Close()

	// TODO: convert to scan
	apps, err := redis.Strings(conn.Do("KEYS", path.Join(r.Env, r.Pool, "*")))
	if err != nil {
		return nil, err
	}

	// TODO: is it OK to error out early?
	var appList []ServiceConfig
	for _, app := range apps {
		parts := strings.Split(app, "/")

		// app entries should be 3 parts, /env/pool/app
		if len(parts) != 3 {
			continue
		}

		// we don't want host keys
		if parts[2] == "hosts" {
			continue
		}

		cfg, err := r.ServiceConfig(app)
		if err != nil {
			return nil, err
		}
		appList = append(appList, *cfg)
	}

	return appList, nil
}

func (r *ServiceRegistry) CreatePool(name string) (bool, error) {
	conn := r.redisPool.Get()
	defer conn.Close()

	//FIXME: Create an associated auto-scaling groups tied to the
	//pool

	added, err := redis.Int(conn.Do("SADD", path.Join(r.Env, "pools", "*"), name))
	if err != nil {
		return false, err
	}
	return added == 1, nil
}

func (r *ServiceRegistry) DeletePool(name string) (bool, error) {
	conn := r.redisPool.Get()
	defer conn.Close()

	//FIXME: Scan keys to make sure there are no deploye apps before
	//deleting the pool.

	//FIXME: Shutdown the associated auto-scaling groups tied to the
	//pool

	removed, err := redis.Int(conn.Do("SREM", path.Join(r.Env, "pools", "*"), name))
	if err != nil {
		return false, err
	}
	return removed == 1, nil
}

func (r *ServiceRegistry) ListPools() ([]string, error) {
	conn := r.redisPool.Get()
	defer conn.Close()

	// TODO: convert to scan
	matches, err := redis.Strings(conn.Do("SMEMBERS", path.Join(r.Env, "pools", "*")))
	if err != nil {
		return nil, err
	}

	return matches, nil
}

func (r *ServiceRegistry) DeleteApp(app string) error {
	conn := r.redisPool.Get()
	defer conn.Close()

	_, err := conn.Do("DEL", path.Join(r.Env, r.Pool, app))
	if err != nil {
		return err
	}

	return nil
}
