package metal

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strconv"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

const (
	apiKeyName                   = "METAL_API_KEY"
	projectIDName                = "METAL_PROJECT_ID"
	facilityName                 = "METAL_FACILITY_NAME"
	loadBalancerSettingName      = "METAL_LB"
	envVarLocalASN               = "METAL_LOCAL_ASN"
	envVarBGPPass                = "METAL_BGP_PASS"
	envVarAnnotationLocalASN     = "METAL_ANNOTATION_LOCAL_ASN"
	envVarAnnotationPeerASNs     = "METAL_ANNOTATION_PEER_ASNS"
	envVarAnnotationPeerIPs      = "METAL_ANNOTATION_PEER_IPS"
	envVarAnnotationSrcIP        = "METAL_ANNOTATION_SRC_IP"
	envVarAnnotationBGPPass      = "METAL_ANNOTATION_BGP_PASS"
	envVarEIPTag                 = "METAL_EIP_TAG"
	envVarAPIServerPort          = "METAL_API_SERVER_PORT"
	envVarBGPNodeSelector        = "METAL_BGP_NODE_SELECTOR"
	defaultLoadBalancerConfigMap = "metallb-system:config"
)

// Config configuration for a provider, includes authentication token, project ID ID, and optional override URL to talk to a different Equinix Metal API endpoint
type Config struct {
	AuthToken           string  `json:"apiKey"`
	ProjectID           string  `json:"projectId"`
	BaseURL             *string `json:"base-url,omitempty"`
	LoadBalancerSetting string  `json:"loadbalancer"`
	Facility            string  `json:"facility,omitempty"`
	LocalASN            int     `json:"localASN,omitempty"`
	BGPPass             string  `json:"bgpPass,omitempty"`
	AnnotationLocalASN  string  `json:"annotationLocalASN,omitEmpty"`
	AnnotationPeerASNs  string  `json:"annotationPeerASNs,omitEmpty"`
	AnnotationPeerIPs   string  `json:"annotationPeerIPs,omitEmpty"`
	AnnotationSrcIP     string  `json:"annotationSrcIP,omitEmpty"`
	AnnotationBGPPass   string  `json:"annotationBGPPass,omitEmpty"`
	EIPTag              string  `json:"eipTag,omitEmpty"`
	APIServerPort       int32   `json:"apiServerPort,omitEmpty"`
	BGPNodeSelector     string  `json:"bgpNodeSelector,omitEmpty"`
}

func AddExtraFlags(fs *pflag.FlagSet, providerConfig *string) {
	fs.StringVar(providerConfig, "provider-config", "", "path to provider config file (DEPRECATED, use cloud-config")
}

// String converts the Config structure to a string, while masking hidden fields.
// Is not 100% a String() conversion, as it adds some intelligence to the output,
// and masks sensitive data
func (c Config) Strings() []string {
	ret := []string{}
	if c.AuthToken != "" {
		ret = append(ret, "authToken: '<masked>'")
	} else {
		ret = append(ret, "authToken: ''")
	}
	ret = append(ret, fmt.Sprintf("projectID: '%s'", c.ProjectID))
	if c.LoadBalancerSetting == "" {
		ret = append(ret, "loadbalancer config: disabled")
	} else {
		ret = append(ret, "load balancer config: ''%s", c.LoadBalancerSetting)
	}
	ret = append(ret, fmt.Sprintf("facility: '%s'", c.Facility))
	ret = append(ret, fmt.Sprintf("local ASN: '%d'", c.LocalASN))
	ret = append(ret, fmt.Sprintf("Elastic IP Tag: '%s'", c.EIPTag))
	ret = append(ret, fmt.Sprintf("API Server Port: '%d'", c.APIServerPort))
	ret = append(ret, fmt.Sprintf("BGP Node Selector: '%s'", c.BGPNodeSelector))

	return ret
}

func getMetalConfig(providerConfig io.Reader) (Config, error) {
	// get our token and project
	var config, rawConfig Config
	configBytes, err := ioutil.ReadAll(providerConfig)
	if err != nil {
		return config, fmt.Errorf("failed to read configuration : %v", err)
	}
	err = json.Unmarshal(configBytes, &rawConfig)
	if err != nil {
		return config, fmt.Errorf("failed to process json of configuration file at path %s: %v", providerConfig, err)
	}

	// read env vars; if not set, use rawConfig
	apiToken := os.Getenv(apiKeyName)
	if apiToken == "" {
		apiToken = rawConfig.AuthToken
	}
	config.AuthToken = apiToken

	projectID := os.Getenv(projectIDName)
	if projectID == "" {
		projectID = rawConfig.ProjectID
	}
	config.ProjectID = projectID

	loadBalancerSetting := os.Getenv(loadBalancerSettingName)
	config.LoadBalancerSetting = rawConfig.LoadBalancerSetting
	// rule for processing: any setting in env var overrides setting from file
	if loadBalancerSetting != "" {
		config.LoadBalancerSetting = loadBalancerSetting
	}
	// and set for default
	if config.LoadBalancerSetting == "" {
		config.LoadBalancerSetting = defaultLoadBalancerConfigMap
	}

	facility := os.Getenv(facilityName)
	if facility == "" {
		facility = rawConfig.Facility
	}

	if apiToken == "" {
		return config, fmt.Errorf("environment variable %q is required", apiKeyName)
	}

	if projectID == "" {
		return config, fmt.Errorf("environment variable %q is required", projectIDName)
	}

	// if facility was not defined, retrieve it from our metadata
	if facility == "" {
		metadata, err := GetAndParseMetadata("")
		if err != nil {
			return config, fmt.Errorf("facility not set in environment variable %q or config file, and error reading metadata: %v", facilityName, err)
		}
		facility = metadata.Facility
	}
	config.Facility = facility

	// get the local ASN
	localASN := os.Getenv(envVarLocalASN)
	switch {
	case localASN != "":
		localASNNo, err := strconv.Atoi(localASN)
		if err != nil {
			return config, fmt.Errorf("env var %s must be a number, was %s: %v", envVarLocalASN, localASN, err)
		}
		config.LocalASN = localASNNo
	case rawConfig.LocalASN != 0:
		config.LocalASN = rawConfig.LocalASN
	default:
		config.LocalASN = DefaultLocalASN
	}

	bgpPass := os.Getenv(envVarBGPPass)
	if bgpPass != "" {
		config.BGPPass = bgpPass
	}

	// set the annotations
	config.AnnotationLocalASN = DefaultAnnotationNodeASN
	annotationLocalASN := os.Getenv(envVarAnnotationLocalASN)
	if annotationLocalASN != "" {
		config.AnnotationLocalASN = annotationLocalASN
	}
	config.AnnotationPeerASNs = DefaultAnnotationPeerASNs
	annotationPeerASNs := os.Getenv(envVarAnnotationPeerASNs)
	if annotationPeerASNs != "" {
		config.AnnotationPeerASNs = annotationPeerASNs
	}
	config.AnnotationPeerIPs = DefaultAnnotationPeerIPs
	annotationPeerIPs := os.Getenv(envVarAnnotationPeerIPs)
	if annotationPeerIPs != "" {
		config.AnnotationPeerIPs = annotationPeerIPs
	}
	config.AnnotationSrcIP = DefaultAnnotationSrcIP
	annotationSrcIP := os.Getenv(envVarAnnotationSrcIP)
	if annotationSrcIP != "" {
		config.AnnotationSrcIP = annotationSrcIP
	}

	config.AnnotationBGPPass = DefaultAnnotationBGPPass
	annotationBGPPass := os.Getenv(envVarAnnotationBGPPass)
	if annotationBGPPass != "" {
		config.AnnotationBGPPass = annotationBGPPass
	}

	if rawConfig.EIPTag != "" {
		config.EIPTag = rawConfig.EIPTag
	}
	eipTag := os.Getenv(envVarEIPTag)
	if eipTag != "" {
		config.EIPTag = eipTag
	}

	apiServer := os.Getenv(envVarAPIServerPort)
	switch {
	case apiServer != "":
		apiServerNo, err := strconv.Atoi(apiServer)
		if err != nil {
			return config, fmt.Errorf("env var %s must be a number, was %s: %v", envVarAPIServerPort, apiServer, err)
		}
		config.APIServerPort = int32(apiServerNo)
	case rawConfig.APIServerPort != 0:
		config.APIServerPort = rawConfig.APIServerPort
	default:
		// if nothing else set it, we set it to 0, to indicate that it should use whatever the kube-apiserver port is
		config.APIServerPort = 0
	}

	config.BGPNodeSelector = rawConfig.BGPNodeSelector
	if v := os.Getenv(envVarBGPNodeSelector); v != "" {
		config.BGPNodeSelector = v
	}

	if _, err := labels.Parse(config.BGPNodeSelector); err != nil {
		return config, fmt.Errorf("BGP Node Selector must be valid Kubernetes selector: %w", err)
	}

	return config, nil
}

// printMetalConfig report the config to startup logs
func printMetalConfig(config Config) {
	lines := config.Strings()
	for _, l := range lines {
		klog.Infof(l)
	}
}
