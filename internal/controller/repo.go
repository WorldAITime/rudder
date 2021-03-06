package controller

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"time"

	"encoding/base64"

	log "github.com/Sirupsen/logrus"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/repo"

	"rudder/internal/util"
)

// RepoController handles helm repository related operations
type RepoController struct {
	repos         []*repo.Entry
	cacheDir      string
	cacheLifetime time.Duration
}

// ChartDetail defines the details of a chart
type ChartDetail struct {
	Metadata  chart.Metadata         `json:"metadata"`
	ValuesRaw string                 `json:"values_raw"`
	Values    map[string]interface{} `json:"values"`
	Templates map[string]string      `json:"templates"`
	ChartURL  string                 `json:"-"`
	ChartFile string                 `json:"-"`
}

// NewRepoController creates a new repo controller.
func NewRepoController(repos []*repo.Entry, cacheDir string, cacheLifetime time.Duration) *RepoController {

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		os.MkdirAll(cacheDir, 0766)
	}

	return &RepoController{
		repos:         repos,
		cacheDir:      cacheDir,
		cacheLifetime: cacheLifetime,
	}
}

// ListRepos returns a list of repositories
func (rc *RepoController) ListRepos() []*repo.Entry {
	return rc.repos
}

func (rc *RepoController) findRepo(repoName string) (r *repo.Entry, err error) {
	for _, rr := range rc.repos {
		if rr.Name == repoName {
			r = rr
			return
		}
	}
	err = errors.New("repository not found")
	return
}

// ListCharts returns the charts contained in the provided repo
func (rc *RepoController) ListCharts(repoName, filter string) (charts map[string]repo.ChartVersions, err error) {
	r, err := rc.findRepo(repoName)
	if err != nil {
		log.WithError(err).Errorf("unable to find repo %s", repoName)
		return
	}
	repoURL := r.URL
	indexURL := repoURL + "/index.yaml"
	data, err := rc.readFromCacheOrURL(indexURL)
	if err != nil {
		log.WithError(err).Error("Unable to get index.yaml from cache or %s", indexURL)
		return
	}
	var index repo.IndexFile
	err = util.YAMLtoJSON(data, &index)
	if err != nil {
		log.WithError(err).Error("Unable to parse index.yaml")
	}
	charts = index.Entries
	filterCharts(charts, filter)
	return
}

// ChartDetail returns the details of the provided chart
func (rc *RepoController) ChartDetails(repoName, chartName, chartVersion string) (chartDetail *ChartDetail, err error) {
	// update charts if needed
	charts, err := rc.ListCharts(repoName, "")
	if err != nil {
		log.WithError(err).Errorf("unable to get list of charts for %s", repoName)
		return
	}
	versions := charts[chartName]
	version, found := findVersion(versions, chartVersion)
	if !found {
		log.Errorf("%s:%s not found", chartName, chartVersion)
		return
	}
	// get the first URL
	chartURL := version.URLs[0]
	data, err := rc.readFromCacheOrURL(chartURL)
	if err != nil {
		log.WithError(err).Errorf("Unable to get chart from cache or %s", chartURL)
		return
	}
	fileMap, err := util.TarballToMap(data)
	if err != nil {
		log.WithError(err).Errorf("Unable to read tarball")
		return
	}

	var m chart.Metadata
	chartYAML := fileMap[chartName+"/Chart.yaml"]
	err = util.YAMLtoJSON(chartYAML, &m)
	if err != nil {
		log.WithError(err).Errorf("Unable to unmarshal chart")
		return
	}

	var v map[string]interface{}
	valuesYAML := fileMap[chartName+"/values.yaml"]
	// vrxp := regexp.MustCompile("# ")
	// valuesYAML = vrxp.ReplaceAll(valuesYAML, []byte(""))
	util.YAMLtoJSON(valuesYAML, &v)
	if err != nil {
		log.WithError(err).Errorf("Unable to unmarshal values")
		return
	}
	valuesRaw := base64.StdEncoding.EncodeToString(valuesYAML)

	regex := regexp.MustCompile(".+/templates/(.+)")
	templates := make(map[string]string)
	for k, v := range fileMap {
		if strings.Contains(k, "templates/") {
			file := regex.FindStringSubmatch(k)[1]
			templates[file] = base64.StdEncoding.EncodeToString(v)
		}
	}

	chartFile := rc.cacheDir + "/" + util.EncodeMD5Hex(chartURL)
	chartDetail = &ChartDetail{
		Metadata:  m,
		ValuesRaw: valuesRaw,
		Values:    v,
		Templates: templates,
		ChartURL:  chartURL,
		ChartFile: chartFile,
	}
	return
}

// readFromCacheOrURL handles reading of the charts. Charts are stored locally for faster access
// but expires at a set time.
func (rc *RepoController) readFromCacheOrURL(url string) ([]byte, error) {
	log.Debugf("Fetching resource from cache or %s...", url)
	mustReload := false

	cacheFile := util.EncodeMD5Hex(url)

	filePath := rc.cacheDir + "/" + cacheFile
	log.Debugf("checking cache: %s", filePath)
	fi, err := os.Stat(filePath)
	if err != nil {
		// file may not exist : needs debug log
		log.Debug("cache not found")
		mustReload = true
	} else {
		// outdated
		log.Debug("cache found")
		mustReload = util.IsOutdated(fi.ModTime(), rc.cacheLifetime)
	}

	if mustReload {
		log.Debug("cache not found or outdated. getting from URL")
		// get from url
		out, err := util.HTTPGet(url)
		if err != nil {
			// unable to download
			log.Debugf("unable to download from %s", url)
			return nil, err
		}
		// save to file
		if err := util.WriteFile(filePath, out); err != nil {
			log.Debugf("unable to save to file %s", filePath)
			return nil, err
		}
	}

	data, err := util.ReadFile(filePath)
	if err != nil {
		log.Debugf("unable to read from %s", filePath)
		return nil, err
	}
	return data, nil
}

func findVersion(versions repo.ChartVersions, version string) (ver *repo.ChartVersion, found bool) {
	for _, v := range versions {
		if v.Version == version {
			ver = v
			found = true
			break
		}
	}
	// if not found but version is latest, return the first item
	if version == "latest" {
		ver = versions[0]
		found = true
	}
	return
}

func filterCharts(charts map[string]repo.ChartVersions, filter string) {
	if filter == "" {
		return
	}
	for key, val := range charts {
		// if key and filter match, skip
		if key == filter {
			continue
		}

		var keywordMatched bool
		for _, versions := range val {
			name := versions.Name
			keywords := versions.Keywords
			// if name matches, skip
			if name == filter {
				continue
			}
			for _, keyword := range keywords {
				if keyword == filter {
					// if keyword matches, skip
					keywordMatched = true
					break
				}
			}
		}
		// if keyword matches, skip (not using labels)
		if keywordMatched {
			continue
		}
		delete(charts, key)
	}
}
