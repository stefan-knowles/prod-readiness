package scanner

import (
	"fmt"
	"strings"
	"time"

	"github.com/coreeng/production-readiness/production-readiness/pkg/k8s"

	"github.com/gammazero/workerpool"
	logr "github.com/sirupsen/logrus"
)

// Scanner will scan images
type Scanner struct {
	config           *Config
	kubernetesClient k8s.KubernetesClient
	dockerClient     DockerClient
	trivyClient      TrivyClient
}

// ScannedImage define the information of an image
type ScannedImage struct {
	TrivyOutputResults   []TrivyOutputResults
	Containers           []k8s.ContainerSummary
	ImageName            string
	ScanError            error
	VulnerabilitySummary VulnerabilitySummary
}

// VulnerabilitySummary provides a summary of the vulnerabilities found for an image
type VulnerabilitySummary struct {
	ContainerCount               int
	SeverityScore                int
	TotalVulnerabilityBySeverity map[string]int
}

// Vulnerabilities is the object representation of the trivy vulnerability table for an image
type Vulnerabilities struct {
	Description      string
	Severity         string
	SeveritySource   string
	FixedVersion     string
	InstalledVersion string
	VulnerabilityID  string
	PkgName          string
	Title            string
	References       []string
	Layer            *Layer
}

// TrivyOutputResults is an object representation of the trivy image scan summary
type TrivyOutputResults struct {
	Vulnerabilities []Vulnerabilities
	Type            string
	Target          string
}

// TrivyOutput is an object representation of the trivy output for an image scan
type TrivyOutput struct {
	Results []TrivyOutputResults
}

// CisOutput is an object representation of the trivy security compliance scan
type CisOutput struct {
	ID               string   `json:"ID"`
	Title            string   `json:"Title"`
	Description      string   `json:"Description"`
	Version          string   `json:"Version"`
	RelatedResources []string `json:"RelatedResources"`
	Results          []struct {
		ID          string `json:"ID"`
		Name        string `json:"Name"`
		Description string `json:"Description"`
		Severity    string `json:"Severity"`
		Results     []struct {
			Target         string `json:"Target"`
			Class          string `json:"Class"`
			Type           string `json:"Type"`
			MisconfSummary struct {
				Successes  int `json:"Successes"`
				Failures   int `json:"Failures"`
				Exceptions int `json:"Exceptions"`
			} `json:"MisconfSummary"`
			Misconfigurations []struct {
				Type        string   `json:"Type"`
				ID          string   `json:"ID"`
				Avdid       string   `json:"AVDID"`
				Title       string   `json:"Title"`
				Description string   `json:"Description"`
				Message     string   `json:"Message"`
				Namespace   string   `json:"Namespace"`
				Query       string   `json:"Query"`
				Resolution  string   `json:"Resolution"`
				Severity    string   `json:"Severity"`
				PrimaryURL  string   `json:"PrimaryURL"`
				References  []string `json:"References"`
				Status      string   `json:"Status"`
				Layer       struct {
				} `json:"Layer"`
				CauseMetadata struct {
					Provider  string `json:"Provider"`
					Service   string `json:"Service"`
					StartLine int    `json:"StartLine"`
					EndLine   int    `json:"EndLine"`
					Code      struct {
						Lines []struct {
							Number     int    `json:"Number"`
							Content    string `json:"Content"`
							IsCause    bool   `json:"IsCause"`
							Annotation string `json:"Annotation"`
							Truncated  bool   `json:"Truncated"`
							FirstCause bool   `json:"FirstCause"`
							LastCause  bool   `json:"LastCause"`
						} `json:"Lines"`
					} `json:"Code"`
				} `json:"CauseMetadata"`
			} `json:"Misconfigurations"`
		} `json:"Results"`
		DefaultStatus string `json:"DefaultStatus,omitempty"`
	} `json:"Results"`
}

// Layer is the object representation of the trivy image layer
type Layer struct {
	DiffID string
	Digest string
}

// Config is the config used for the scanner
type Config struct {
	LogLevel             string
	Workers              int
	ImageNameReplacement string
	AreaLabels           string
	TeamsLabels          string
	FilterLabels         string
	Severity             string
	ScanImageTimeout     time.Duration
}

// New creates a Scanner to find vulnerabilities in container images
func New(kubernetesClient k8s.KubernetesClient, config *Config) *Scanner {
	return &Scanner{
		config:           config,
		kubernetesClient: kubernetesClient,
		dockerClient:     NewDockerClient(),
		trivyClient:      NewTrivyClient(config.Severity, config.ScanImageTimeout),
	}
}

// ScanImages get all the images available in a cluster and scan them
func (s *Scanner) ScanImages() (*VulnerabilityReport, error) {
	logr.Infof("Running scanner")
	containers, err := s.kubernetesClient.GetContainersInNamespaces(s.config.FilterLabels)
	if err != nil {
		return nil, err
	}
	containersByImageName := s.groupContainersByImageName(containers)
	scannedImages, err := s.scanImages(containersByImageName)
	if err != nil {
		return nil, err
	}

	logr.Infof("Generating vulnerability report")
	reportGenerator := &AreaReport{
		AreaLabelName: s.config.AreaLabels,
		TeamLabelName: s.config.TeamsLabels,
	}
	return reportGenerator.GenerateVulnerabilityReport(scannedImages)
}

func (s *Scanner) groupContainersByImageName(containers []k8s.ContainerSummary) map[string][]k8s.ContainerSummary {
	images := make(map[string][]k8s.ContainerSummary)
	for _, container := range containers {
		if _, ok := images[container.Image]; !ok {
			images[container.Image] = []k8s.ContainerSummary{}
		}
		images[container.Image] = append(images[container.Image], container)
	}
	return images
}

func (s *Scanner) scanImages(imageList map[string][]k8s.ContainerSummary) ([]ScannedImage, error) {
	var scannedImages []ScannedImage
	wp := workerpool.New(s.config.Workers)
	err := s.trivyClient.DownloadDatabase("image")
	if err != nil {
		return nil, fmt.Errorf("failed to download trivy db: %v", err)
	}

	logr.Infof("Scanning %d images with %d workers", len(imageList), s.config.Workers)
	for imageName, containers := range imageList {
		// allocate var to allow access inside the worker submission
		resolvedContainers := containers
		resolvedImageName, err := s.stringReplacement(imageName, s.config.ImageNameReplacement)
		if err != nil {
			logr.Errorf("Error string replacement failed, image_name : %s, image_replacement_string: %s, error: %s", imageName, s.config.ImageNameReplacement, err)
		}

		wp.Submit(func() {
			logr.Infof("Worker processing image: %s", resolvedImageName)

			// trivy fail to download from quay.io so we need to pull the image first
			err := s.dockerClient.PullImage(resolvedImageName)
			if err != nil {
				logr.Errorf("Error executing docker pull for image %s: %v", resolvedImageName, err)
			}

			trivyOutput, err := s.trivyClient.ScanImage(resolvedImageName)
			var scanError error
			if err != nil {
				scanError = fmt.Errorf("error executing trivy for image %s: %s", resolvedImageName, err)
				logr.Error(scanError)
			}
			scannedImages = append(scannedImages, NewScannedImage(
				resolvedImageName,
				resolvedContainers,
				trivyOutput,
				scanError,
			))

			err = s.dockerClient.RmiImage(resolvedImageName)
			if err != nil {
				logr.Errorf("Error executing docker rmi for image %s: %v", resolvedImageName, err)
			}
		})
	}

	wp.StopWait()
	return scannedImages, nil
}

// CisScan perform trivy compliance scan
func (s *Scanner) CisScan(benchmark string) (*VulnerabilityReport, error) {
	logr.Infof("Running %s security benchmark", benchmark)

	trivyOutput, err := s.trivyClient.CisScan(benchmark)
	if err != nil {
		return nil, fmt.Errorf("error executing trivy cluster scan: %v", err)
	}

	logr.Infof("SUCCESS: %v", trivyOutput)

	logr.Infof("Generating %s security benchmark report", benchmark)
	reportGenerator := &AreaReport{
		AreaLabelName: s.config.AreaLabels,
		TeamLabelName: s.config.TeamsLabels,
	}
	return reportGenerator.GenerateVulnerabilityReport(nil)
}

const (
	critical = 100000000
	high     = 1000000
	medium   = 10000
	low      = 100
	unknown  = 1
)

var severityScores = map[string]int{
	"CRITICAL": critical, "HIGH": high, "MEDIUM": medium, "LOW": low, "UNKNOWN": unknown,
}

// NewScannedImage created a new ScannedImage with all fields initialised
func NewScannedImage(imageName string, containers []k8s.ContainerSummary, trivyOutput []TrivyOutputResults, scanError error) ScannedImage {
	i := ScannedImage{
		ImageName:          imageName,
		Containers:         containers,
		TrivyOutputResults: trivyOutput,
		ScanError:          scanError,
	}
	i.VulnerabilitySummary = i.buildVulnerabilitySummary()
	return i
}

func (i *ScannedImage) buildVulnerabilitySummary() VulnerabilitySummary {
	severityMap := make(map[string]int)
	for severity := range severityScores {
		severityMap[severity] = 0
	}
	for _, target := range i.TrivyOutputResults {
		for _, vulnerability := range target.Vulnerabilities {
			severityMap[vulnerability.Severity] = severityMap[vulnerability.Severity] + 1
		}
	}

	severityScore := 0
	for severity, count := range severityMap {
		score := severityScores[severity]
		severityScore = severityScore + count*score
	}
	return VulnerabilitySummary{
		ContainerCount:               len(i.Containers),
		SeverityScore:                severityScore,
		TotalVulnerabilityBySeverity: severityMap,
	}
}

func (s *Scanner) stringReplacement(imageName string, stringReplacement string) (string, error) {
	if stringReplacement != "" {
		replacementArr := strings.Split(stringReplacement, ",")
		for _, pattern := range replacementArr {

			replacementItems := strings.Split(pattern, "|")
			if len(replacementItems) == 2 {
				logr.Debugf("String replacement from imageName: %s, match: %s, replace %s", imageName, replacementItems[0], replacementItems[1])
				imageName = strings.Replace(imageName, replacementItems[0], replacementItems[1], -1)
			} else {
				return imageName, fmt.Errorf("string Replacement pattern is not in the right format '$matchingString|$replacementString,$matchingString|$replacementString'")
			}

		}
	}
	return imageName, nil
}
