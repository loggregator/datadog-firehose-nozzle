package parser

import (
	"fmt"
	"sync"
	"time"

	"encoding/json"
	"github.com/DataDog/datadog-firehose-nozzle/internal/metric"
	"github.com/DataDog/datadog-firehose-nozzle/internal/util"
	"github.com/cloudfoundry-community/go-cfclient"
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/sonde-go/events"
	bolt "github.com/coreos/bbolt"
)

var clearCacheDuration = 60

type AppParser struct {
	CFClient     *cfclient.Client
	log          *gosteno.Logger
	Apps         map[string]*App
	appLock      sync.RWMutex
	db           *bolt.DB
	grabInterval int
	customTags   []string
	appBucket    []byte
}

func NewAppParser(
	cfClient *cfclient.Client,
	grabInterval int,
	log *gosteno.Logger,
	customTags []string,
	db *bolt.DB,
	environment string,
) (*AppParser, error) {

	if cfClient == nil {
		return nil, fmt.Errorf("The CF Client needs to be properly set up to use appmetrics")
	}
	if environment != "" {
		customTags = append(customTags, fmt.Sprintf("%s:%s", "env", environment))
	}
	appMetrics := &AppParser{
		CFClient:     cfClient,
		log:          log,
		Apps:         make(map[string]*App),
		grabInterval: grabInterval,
		customTags:   customTags,
		appBucket:    []byte("CloudFoundryApps"),
		db:           db,
	}

	// create the cache db or grab the app cache from it
	appMetrics.reloadCache()
	// start the background loop to keep the cache up to date
	go appMetrics.updateCacheLoop()

	return appMetrics, nil
}

func (am *AppParser) updateCacheLoop() {
	// If an app hasn't sent a metric in a while,
	// assume that it's either been taken down or
	// that the loggregator is routing it to a different nozzle and remove it from the cache
	ticker := time.NewTicker(time.Duration(clearCacheDuration) * time.Minute)
	for {
		select {
		case <-ticker.C:
			var toRemove = []string{}
			var oneHourAgo = (time.Now().Add(-time.Duration(clearCacheDuration) * time.Minute)).Unix()
			am.appLock.Lock()
			updatedApps := make(map[string][]byte)
			for guid, app := range am.Apps {
				app.lock.RLock()
				if app.updated < oneHourAgo {
					toRemove = append(toRemove, guid)
				} else {
					jsonApp, err := json.Marshal(app)
					if err != nil {
						am.log.Infof("Error marshalling app for database: %v", err)
					}
					updatedApps[guid] = jsonApp
				}
				app.lock.RUnlock()
			}
			for _, guid := range toRemove {
				delete(am.Apps, guid)
			}
			am.appLock.Unlock()

			// update the database after closing the app map
			// since this won't affect the app map, no need to continue touching it
			am.db.Batch(func(tx *bolt.Tx) error {
				b, err := tx.CreateBucketIfNotExists(am.appBucket)
				if err != nil {
					return fmt.Errorf("create bucket: %s", err)
				}
				// delete removed apps
				for _, guid := range toRemove {
					b.Delete([]byte(guid))
				}
				// update modified apps
				for guid, jsonApp := range updatedApps {
					b.Put([]byte(guid), jsonApp)
				}
				return nil
			})
		}
	}
}

func (am *AppParser) reloadCache() error {
	err := am.db.Batch(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(am.appBucket)
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}
		if b == nil {
			return fmt.Errorf("bucket not created")
		}

		am.appLock.Lock()
		defer am.appLock.Unlock()

		return b.ForEach(func(k []byte, v []byte) error {
			guid := string(k)
			var app *App
			err := json.Unmarshal(v, app)
			if err != nil {
				return err
			}
			am.Apps[guid] = app

			return nil
		})
	})

	return err
}

func (am *AppParser) getAppData(guid string) (*App, error) {
	am.appLock.Lock()
	defer am.appLock.Unlock()

	var app *App
	if _, ok := am.Apps[guid]; ok {
		// If it exists in the cache, use the cache
		app = am.Apps[guid]
		timeToGrab := (time.Now().Add(-time.Duration(am.grabInterval) * time.Minute)).Unix()
		if !app.ErrorGrabbing && app.updated > timeToGrab {
			return app, nil
		}
	} else {
		am.Apps[guid] = newApp(guid)
		app = am.Apps[guid]
	}
	app.lock.Lock()
	defer app.lock.Unlock()

	resolvedApp, err := am.CFClient.AppByGuid(guid)
	if err != nil {
		if app.ErrorGrabbing {
			// If there was a previous error grabbing the app, assume it's been removed and remove it from the cache
			am.log.Errorf("there was an error grabbing the instance data for app %v, removing from cache: %v", resolvedApp.Guid, err)
			delete(am.Apps, guid)
		} else {
			// If there was not, say that there was such an error
			am.log.Errorf("there was an error grabbing the instance data for app %v: %v", resolvedApp.Guid, err)
		}
		// Ensure that ErrorGrabbing is set
		app.ErrorGrabbing = true
		return nil, err
	}

	app.ErrorGrabbing = false
	app.updated = time.Now().Unix()

	// See https://apidocs.cloudfoundry.org/9.0.0/apps/retrieve_a_particular_app.html for the description of attributes
	app.Name = resolvedApp.Name
	if app.Name == "" {
		am.log.Infof("App %v has no name", guid)
	}
	if resolvedApp.Buildpack != "" {
		app.Buildpack = resolvedApp.Buildpack
	} else if resolvedApp.DetectedBuildpack != "" {
		app.Buildpack = resolvedApp.DetectedBuildpack
	}
	app.Command = resolvedApp.Command
	app.DockerImage = resolvedApp.DockerImage
	app.Diego = resolvedApp.Diego
	app.SpaceID = resolvedApp.SpaceGuid

	resolvedInstances, err := am.CFClient.GetAppInstances(guid)
	if err == nil {
		app.Instances = make(map[string]Instance)
		app.NumberOfInstances = len(resolvedInstances)
		for i, inst := range resolvedInstances {
			app.Instances[i] = Instance{
				InstanceIndex: i,
				State:         inst.State,
			}
		}
	} else {
		am.log.Errorf("there was an error grabbing the instance data for app %v: %v", resolvedApp.Guid, err)
	}

	app.TotalDiskConfigured = resolvedApp.DiskQuota
	app.TotalMemoryConfigured = resolvedApp.Memory
	app.TotalDiskProvisioned = resolvedApp.DiskQuota * app.NumberOfInstances
	app.TotalMemoryProvisioned = resolvedApp.Memory * app.NumberOfInstances

	space, err := resolvedApp.Space()
	if err == nil {
		app.SpaceName = space.Name
		org, e := space.Org()
		if e == nil {
			app.OrgName = org.Name
			app.OrgID = org.Guid
		} else {
			app.ErrorGrabbing = true
			am.log.Errorf("there was an error grabbing the org data for app %v in space %v: %v", resolvedApp.Guid, space.Guid, e)
		}
	} else {
		app.ErrorGrabbing = true
		am.log.Errorf("there was an error grabbing the space data for app %v: %v", resolvedApp.Guid, err)
	}

	app.Tags = app.generateTags()
	return app, nil
}

func (am *AppParser) Parse(envelope *events.Envelope) ([]metric.MetricPackage, error) {
	metricsPackages := []metric.MetricPackage{}
	message := envelope.GetContainerMetric()

	guid := message.GetApplicationId()
	app, err := am.getAppData(guid)
	if err != nil || app == nil {
		am.log.Errorf("there was an error grabbing data for app %v: %v", guid, err)
		return metricsPackages, err
	}

	app.lock.Lock()
	defer app.lock.Unlock()

	app.Host = envelope.GetOrigin()

	metricsPackages = app.getMetrics(am.customTags)
	containerMetrics, err := app.parseContainerMetric(message, am.customTags)
	if err != nil {
		return metricsPackages, err
	}
	metricsPackages = append(metricsPackages, containerMetrics...)

	return metricsPackages, nil
}

type App struct {
	Name                   string
	Host                   string
	Buildpack              string
	Command                string
	Diego                  bool
	OrgName                string
	OrgID                  string
	Routes                 []string
	SpaceID                string
	SpaceName              string
	SpaceURL               string
	GUID                   string
	DockerImage            string
	Instances              map[string]Instance
	NumberOfInstances      int
	TotalDiskConfigured    int
	TotalMemoryConfigured  int
	TotalDiskProvisioned   int
	TotalMemoryProvisioned int
	ErrorGrabbing          bool
	Tags                   []string
	updated                int64
	lock                   sync.RWMutex
}

type Instance struct {
	CellIP        string
	State         string
	InstanceIndex string
}

func newApp(guid string) *App {
	return &App{
		GUID:    guid,
		updated: time.Now().Unix(),
	}
}

func (a *App) getMetrics(customTags []string) []metric.MetricPackage {
	var names = []string{
		"app.disk.configured",
		"app.disk.provisioned",
		"app.memory.configured",
		"app.memory.provisioned",
		"app.instances",
	}

	var ms = []float64{
		float64(a.TotalDiskConfigured),
		float64(a.TotalDiskProvisioned),
		float64(a.TotalMemoryConfigured),
		float64(a.TotalMemoryProvisioned),
		float64(a.NumberOfInstances),
	}

	return a.mkMetrics(names, ms, customTags)
}

func (a *App) parseContainerMetric(message *events.ContainerMetric, customTags []string) ([]metric.MetricPackage, error) {
	var names = []string{
		"app.cpu.pct",
		"app.disk.used",
		"app.disk.quota",
		"app.memory.used",
		"app.memory.quota",
	}
	var ms = []float64{
		float64(message.GetCpuPercentage()),
		float64(message.GetDiskBytes()),
		float64(message.GetDiskBytesQuota()),
		float64(message.GetMemoryBytes()),
		float64(message.GetMemoryBytesQuota()),
	}
	tags := []string{fmt.Sprintf("instance:%v", message.GetInstanceIndex())}
	tags = append(tags, customTags...)

	return a.mkMetrics(names, ms, tags), nil
}

func (a *App) mkMetrics(names []string, ms []float64, moreTags []string) []metric.MetricPackage {
	metricsPackages := []metric.MetricPackage{}
	var host string
	if a.Host != "" {
		host = a.Host
	} else {
		host = a.GUID
	}

	tags := a.getTags()
	tags = append(tags, moreTags...)

	for i, name := range names {
		key := metric.MetricKey{
			Name:     name,
			TagsHash: util.HashTags(tags),
		}
		mVal := metric.MetricValue{
			Tags: tags,
			Host: host,
		}
		p := metric.Point{
			Timestamp: time.Now().Unix(),
			Value:     float64(ms[i]),
		}
		mVal.Points = append(mVal.Points, p)
		metricsPackages = append(metricsPackages, metric.MetricPackage{
			MetricKey:   &key,
			MetricValue: &mVal,
		})
	}

	return metricsPackages
}

func (a *App) getTags() []string {
	if a.Tags != nil && len(a.Tags) > 0 {
		return a.Tags
	}

	a.Tags = a.generateTags()
	return a.Tags
}

func (a *App) generateTags() []string {
	var tags = []string{}
	if a.Name != "" {
		tags = append(tags, fmt.Sprintf("app_name:%v", a.Name))
	}
	if a.Buildpack != "" {
		tags = append(tags, fmt.Sprintf("buildpack:%v", a.Buildpack))
	}
	if a.Command != "" {
		tags = append(tags, fmt.Sprintf("command:%v", a.Command))
	}
	if a.Diego {
		tags = append(tags, fmt.Sprintf("diego"))
	}
	if a.OrgName != "" {
		tags = append(tags, fmt.Sprintf("org_name:%v", a.OrgName))
	}
	if a.OrgID != "" {
		tags = append(tags, fmt.Sprintf("org_id:%v", a.OrgID))
	}
	if a.SpaceName != "" {
		tags = append(tags, fmt.Sprintf("space_name:%v", a.SpaceName))
	}
	if a.SpaceID != "" {
		tags = append(tags, fmt.Sprintf("space_id:%v", a.SpaceID))
	}
	if a.SpaceURL != "" {
		tags = append(tags, fmt.Sprintf("space_url:%v", a.SpaceURL))
	}
	if a.GUID != "" {
		tags = append(tags, fmt.Sprintf("guid:%v", a.GUID))
	}
	if a.DockerImage != "" {
		tags = append(tags, fmt.Sprintf("image:%v", a.DockerImage))
	}

	return tags
}
