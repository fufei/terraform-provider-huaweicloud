package huaweicloud

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/hashicorp/errwrap"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/terraform-plugin-sdk/helper/logging"
	"github.com/hashicorp/terraform-plugin-sdk/helper/pathorcontents"
	"github.com/hashicorp/terraform-plugin-sdk/httpclient"
	"github.com/huaweicloud/golangsdk"
	huaweisdk "github.com/huaweicloud/golangsdk/openstack"
	"github.com/huaweicloud/golangsdk/openstack/identity/v3/projects"
	"github.com/huaweicloud/golangsdk/openstack/obs"
)

const (
	obsLogFile         string = "./.obs-sdk.log"
	obsLogFileSize10MB int64  = 1024 * 1024 * 10
)

type Config struct {
	AccessKey           string
	SecretKey           string
	CACertFile          string
	ClientCertFile      string
	ClientKeyFile       string
	DomainID            string
	DomainName          string
	IdentityEndpoint    string
	Insecure            bool
	Password            string
	Region              string
	TenantID            string
	TenantName          string
	Token               string
	Username            string
	UserID              string
	AgencyName          string
	AgencyDomainName    string
	DelegatedProject    string
	Cloud               string
	MaxRetries          int
	TerraformVersion    string
	RegionClient        bool
	EnterpriseProjectID string

	HwClient *golangsdk.ProviderClient
	s3sess   *session.Session

	DomainClient *golangsdk.ProviderClient

	// RegionProjectIDMap is a map which stores the region-projectId pairs,
	// and region name will be the key and projectID will be the value in this map.
	RegionProjectIDMap map[string]string

	// RPLock is used to make the accessing of RegionProjectIDMap serial,
	// prevent sending duplicate query requests
	RPLock *sync.Mutex
}

func (c *Config) LoadAndValidate() error {
	if c.MaxRetries < 0 {
		return fmt.Errorf("max_retries should be a positive value")
	}

	err := fmt.Errorf("Must config token or aksk or username password to be authorized")

	if c.Token != "" {
		err = buildClientByToken(c)

	} else if c.AccessKey != "" && c.SecretKey != "" {
		err = buildClientByAKSK(c)

	} else if c.Password != "" {
		if c.Username == "" && c.UserID == "" {
			err = fmt.Errorf("\"password\": one of `user_name, user_id` must be specified")
		} else {
			err = buildClientByPassword(c)
		}

	}
	if err != nil {
		return err
	}

	return c.newS3Session(logging.IsDebugOrHigher())
}

func generateTLSConfig(c *Config) (*tls.Config, error) {
	config := &tls.Config{}
	if c.CACertFile != "" {
		caCert, _, err := pathorcontents.Read(c.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("Error reading CA Cert: %s", err)
		}

		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM([]byte(caCert))
		config.RootCAs = caCertPool
	}

	if c.Insecure {
		config.InsecureSkipVerify = true
	}

	if c.ClientCertFile != "" && c.ClientKeyFile != "" {
		clientCert, _, err := pathorcontents.Read(c.ClientCertFile)
		if err != nil {
			return nil, fmt.Errorf("Error reading Client Cert: %s", err)
		}
		clientKey, _, err := pathorcontents.Read(c.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("Error reading Client Key: %s", err)
		}

		cert, err := tls.X509KeyPair([]byte(clientCert), []byte(clientKey))
		if err != nil {
			return nil, err
		}

		config.Certificates = []tls.Certificate{cert}
		config.BuildNameToCertificate()
	}

	return config, nil
}

func genClient(c *Config, ao golangsdk.AuthOptionsProvider) (*golangsdk.ProviderClient, error) {
	client, err := huaweisdk.NewClient(ao.GetIdentityEndpoint())
	if err != nil {
		return nil, err
	}

	// Set UserAgent
	client.UserAgent.Prepend(httpclient.TerraformUserAgent(c.TerraformVersion))

	config, err := generateTLSConfig(c)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: config}

	client.HTTPClient = http.Client{
		Transport: &LogRoundTripper{
			Rt:         transport,
			OsDebug:    logging.IsDebugOrHigher(),
			MaxRetries: c.MaxRetries,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if client.AKSKAuthOptions.AccessKey != "" {
				golangsdk.ReSign(req, golangsdk.SignOptions{
					AccessKey: client.AKSKAuthOptions.AccessKey,
					SecretKey: client.AKSKAuthOptions.SecretKey,
				})
			}
			return nil
		},
	}

	// Validate authentication normally.
	err = huaweisdk.Authenticate(client, ao)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func (c *Config) newS3Session(osDebug bool) error {

	if c.AccessKey != "" && c.SecretKey != "" {
		// Setup S3 client/config information for Swift S3 buckets
		log.Println("[INFO] Building Swift S3 auth structure")
		creds, err := GetCredentials(c)
		if err != nil {
			return err
		}
		// Call Get to check for credential provider. If nothing found, we'll get an
		// error, and we can present it nicely to the user
		cp, err := creds.Get()
		if err != nil {
			if sErr, ok := err.(awserr.Error); ok && sErr.Code() == "NoCredentialProviders" {
				return fmt.Errorf("No valid credential sources found for S3 Provider.")
			}

			return fmt.Errorf("Error loading credentials for S3 Provider: %s", err)
		}

		log.Printf("[INFO] S3 Auth provider used: %q", cp.ProviderName)

		sConfig := &aws.Config{
			Credentials: creds,
			Region:      aws.String(c.Region),
			HTTPClient:  cleanhttp.DefaultClient(),
		}

		if osDebug {
			sConfig.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody | aws.LogDebugWithRequestRetries | aws.LogDebugWithRequestErrors)
			sConfig.Logger = sLogger{}
		}

		if c.Insecure {
			transport := sConfig.HTTPClient.Transport.(*http.Transport)
			transport.TLSClientConfig = &tls.Config{
				InsecureSkipVerify: true,
			}
		}

		// Set up base session for S3
		c.s3sess, err = session.NewSession(sConfig)
		if err != nil {
			return errwrap.Wrapf("Error creating Swift S3 session: {{err}}", err)
		}
	}

	return nil
}

func buildClientByToken(c *Config) error {
	var pao, dao golangsdk.AuthOptions

	if c.AgencyDomainName != "" && c.AgencyName != "" {
		pao = golangsdk.AuthOptions{
			AgencyName:       c.AgencyName,
			AgencyDomainName: c.AgencyDomainName,
			DelegatedProject: c.DelegatedProject,
		}

		dao = golangsdk.AuthOptions{
			AgencyName:       c.AgencyName,
			AgencyDomainName: c.AgencyDomainName,
		}
	} else {
		pao = golangsdk.AuthOptions{
			DomainID:   c.DomainID,
			DomainName: c.DomainName,
			TenantID:   c.TenantID,
			TenantName: c.TenantName,
		}

		dao = golangsdk.AuthOptions{
			DomainID:   c.DomainID,
			DomainName: c.DomainName,
		}
	}

	for _, ao := range []*golangsdk.AuthOptions{&pao, &dao} {
		ao.IdentityEndpoint = c.IdentityEndpoint
		ao.TokenID = c.Token

	}
	return genClients(c, pao, dao)
}

func buildClientByAKSK(c *Config) error {
	var pao, dao golangsdk.AKSKAuthOptions

	if c.AgencyDomainName != "" && c.AgencyName != "" {
		pao = golangsdk.AKSKAuthOptions{
			DomainID:         c.DomainID,
			Domain:           c.DomainName,
			AgencyName:       c.AgencyName,
			AgencyDomainName: c.AgencyDomainName,
			DelegatedProject: c.DelegatedProject,
		}

		dao = golangsdk.AKSKAuthOptions{
			DomainID:         c.DomainID,
			Domain:           c.DomainName,
			AgencyName:       c.AgencyName,
			AgencyDomainName: c.AgencyDomainName,
		}
	} else {
		pao = golangsdk.AKSKAuthOptions{
			BssDomainID: c.DomainID,
			BssDomain:   c.DomainName,
			ProjectName: c.TenantName,
			ProjectId:   c.TenantID,
		}

		dao = golangsdk.AKSKAuthOptions{
			DomainID: c.DomainID,
			Domain:   c.DomainName,
		}
	}

	for _, ao := range []*golangsdk.AKSKAuthOptions{&pao, &dao} {
		ao.IdentityEndpoint = c.IdentityEndpoint
		ao.AccessKey = c.AccessKey
		ao.SecretKey = c.SecretKey
	}
	return genClients(c, pao, dao)
}

func buildClientByPassword(c *Config) error {
	var pao, dao golangsdk.AuthOptions

	if c.AgencyDomainName != "" && c.AgencyName != "" {
		pao = golangsdk.AuthOptions{
			DomainID:         c.DomainID,
			DomainName:       c.DomainName,
			AgencyName:       c.AgencyName,
			AgencyDomainName: c.AgencyDomainName,
			DelegatedProject: c.DelegatedProject,
		}

		dao = golangsdk.AuthOptions{
			DomainID:         c.DomainID,
			DomainName:       c.DomainName,
			AgencyName:       c.AgencyName,
			AgencyDomainName: c.AgencyDomainName,
		}
	} else {
		pao = golangsdk.AuthOptions{
			DomainID:   c.DomainID,
			DomainName: c.DomainName,
			TenantID:   c.TenantID,
			TenantName: c.TenantName,
		}

		dao = golangsdk.AuthOptions{
			DomainID:   c.DomainID,
			DomainName: c.DomainName,
		}
	}

	for _, ao := range []*golangsdk.AuthOptions{&pao, &dao} {
		ao.IdentityEndpoint = c.IdentityEndpoint
		ao.Password = c.Password
		ao.Username = c.Username
		ao.UserID = c.UserID
	}
	return genClients(c, pao, dao)
}

func genClients(c *Config, pao, dao golangsdk.AuthOptionsProvider) error {
	client, err := genClient(c, pao)
	if err != nil {
		return err
	}
	c.HwClient = client

	client, err = genClient(c, dao)
	if err == nil {
		c.DomainClient = client
	}
	return err
}

type sLogger struct{}

func (l sLogger) Log(args ...interface{}) {
	tokens := make([]string, 0, len(args))
	for _, arg := range args {
		if token, ok := arg.(string); ok {
			tokens = append(tokens, token)
		}
	}
	log.Printf("[DEBUG] [aws-sdk-go] %s", strings.Join(tokens, " "))
}

func getObsEndpoint(c *Config, region string) string {
	return fmt.Sprintf("https://obs.%s.%s/", region, c.Cloud)
}

func (c *Config) computeS3conn(region string) (*s3.S3, error) {
	if c.s3sess == nil {
		return nil, fmt.Errorf("missing credentials for Swift S3 Provider, need access_key and secret_key values for provider")
	}

	obsEndpoint := getObsEndpoint(c, region)
	S3Sess := c.s3sess.Copy(&aws.Config{Endpoint: aws.String(obsEndpoint)})
	s3conn := s3.New(S3Sess)

	return s3conn, nil
}

func (c *Config) newObjectStorageClientWithSignature(region string) (*obs.ObsClient, error) {
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, fmt.Errorf("missing credentials for OBS, need access_key and secret_key values for provider")
	}

	// init log
	if logging.IsDebugOrHigher() {
		if err := obs.InitLog(obsLogFile, obsLogFileSize10MB, 10, obs.LEVEL_DEBUG, false); err != nil {
			log.Printf("[WARN] initial obs sdk log failed: %s", err)
		}
	}

	obsEndpoint := getObsEndpoint(c, region)
	return obs.New(c.AccessKey, c.SecretKey, obsEndpoint, obs.WithSignature("OBS"))
}

func (c *Config) newObjectStorageClient(region string) (*obs.ObsClient, error) {
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, fmt.Errorf("missing credentials for OBS, need access_key and secret_key values for provider")
	}

	// init log
	if logging.IsDebugOrHigher() {
		if err := obs.InitLog(obsLogFile, obsLogFileSize10MB, 10, obs.LEVEL_DEBUG, false); err != nil {
			log.Printf("[WARN] initial obs sdk log failed: %s", err)
		}
	}

	obsEndpoint := getObsEndpoint(c, region)
	return obs.New(c.AccessKey, c.SecretKey, obsEndpoint)
}

// NewServiceClient create a ServiceClient which was assembled from ServiceCatalog.
// If you want to add new ServiceClient, please make sure the catalog was already in allServiceCatalog.
// the endpoint likes https://{Name}.{Region}.myhuaweicloud.com/{Version}/{project_id}/{ResourceBase}
func (c *Config) NewServiceClient(srv, region string) (*golangsdk.ServiceClient, error) {
	client := c.HwClient
	if allServiceCatalog[srv].Admin {
		client = c.DomainClient
	}
	return c.newServiceClientByName(client, allServiceCatalog[srv], region)
}

func (c *Config) newServiceClientByName(client *golangsdk.ProviderClient, catalog ServiceCatalog, region string) (*golangsdk.ServiceClient, error) {
	if catalog.Name == "" || catalog.Version == "" {
		return nil, fmt.Errorf("must specify the service name and api version")
	}

	// Custom Resource-level region only supports AK/SK authentication.
	// If set it when using non AK/SK authentication, then it must be the same as Provider-level region.
	if region != c.Region && (c.AccessKey == "" || c.SecretKey == "") {
		return nil, fmt.Errorf("Resource-level region must be the same as Provider-level region when using non AK/SK authentication if Resource-level region set")
	}

	c.RPLock.Lock()
	defer c.RPLock.Unlock()
	projectID, ok := c.RegionProjectIDMap[region]
	if !ok {
		// Not find in the map, then try to query and store.
		err := c.loadUserProjects(client, region)
		if err != nil {
			return nil, err
		}
		projectID, _ = c.RegionProjectIDMap[region]
	}

	sc := new(golangsdk.ServiceClient)

	clone := new(golangsdk.ProviderClient)
	*clone = *client
	clone.ProjectID = projectID
	clone.AKSKAuthOptions.ProjectId = projectID
	clone.AKSKAuthOptions.Region = region
	sc.ProviderClient = clone

	if catalog.Scope == "global" && !c.RegionClient {
		sc.Endpoint = fmt.Sprintf("https://%s.%s/", catalog.Name, c.Cloud)
	} else {
		sc.Endpoint = fmt.Sprintf("https://%s.%s.%s/", catalog.Name, region, c.Cloud)
	}

	sc.ResourceBase = sc.Endpoint + catalog.Version + "/"
	if !catalog.WithOutProjectID {
		sc.ResourceBase = sc.ResourceBase + projectID + "/"
	}
	if catalog.ResourceBase != "" {
		sc.ResourceBase = sc.ResourceBase + catalog.ResourceBase + "/"
	}

	return sc, nil
}

// loadUserProjects will query the region-projectId pair and store it into RegionProjectIDMap
func (c *Config) loadUserProjects(client *golangsdk.ProviderClient, region string) error {

	log.Printf("Load projectID for region: %s", region)
	domainID := client.DomainID
	opts := projects.ListOpts{
		DomainID: domainID,
		Name:     region,
	}
	sc := new(golangsdk.ServiceClient)
	sc.Endpoint = c.IdentityEndpoint + "/"
	sc.ProviderClient = client
	allPages, err := projects.List(sc, &opts).AllPages()
	if err != nil {
		return fmt.Errorf("List projects failed, err=%s", err)
	}

	all, err := projects.ExtractProjects(allPages)
	if err != nil {
		return fmt.Errorf("Extract projects failed, err=%s", err)
	}

	if len(all) == 0 {
		return fmt.Errorf("Wrong name or no access to the region: %s", region)
	}

	for _, item := range all {
		c.RegionProjectIDMap[item.Name] = item.ID
	}
	return nil
}

// ********** client for Global Service **********
func (c *Config) IAMV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("iam", region)
}

func (c *Config) IdentityV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("identity", region)
}

func (c *Config) DnsV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("dns", region)
}

func (c *Config) CdnV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("cdn", region)
}

func (c *Config) EnterpriseProjectClient(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("eps", region)
}

// ********** client for Compute **********
func (c *Config) computeV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("ecs", region)
}

func (c *Config) computeV11Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("ecsv11", region)
}

func (c *Config) computeV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("ecsv21", region)
}

func (c *Config) autoscalingV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("autoscalingv1", region)
}

func (c *Config) imageV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("imagev2", region)
}

func (c *Config) cceV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("ccev3", region)
}

func (c *Config) cceAddonV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("cceaddonv3", region)
}

func (c *Config) cciV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("cciv1", region)
}

func (c *Config) FgsV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("fgsv2", region)
}

// ********** client for Storage **********
func (c *Config) blockStorageV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("volumev2", region)
}

func (c *Config) blockStorageV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("volumev3", region)
}

func (c *Config) sfsV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("sfsv2", region)
}

func (c *Config) sfsV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("sfs-turbo", region)
}

func (c *Config) csbsV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("csbsv1", region)
}

func (c *Config) vbsV2Client(region string) (*golangsdk.ServiceClient, error) {

	return c.NewServiceClient("vbsv2", region)
}

// ********** client for Network **********
func (c *Config) NetworkingV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("vpc", region)
}

func (c *Config) SecurityGroupV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("security_group", region)
}

func (c *Config) NetworkingV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("networkv2", region)
}

func (c *Config) natV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("natv2", region)
}

func (c *Config) natGatewayV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("nat_gatewayv2", region)
}

func (c *Config) elasticLBClient(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("elb", region)
}

func (c *Config) elbV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("elbv2", region)
}

func (c *Config) fwV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("networkv2", region)
}

// ********** client for Management **********
func (c *Config) ctsV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("cts", region)
}

func (c *Config) newCESClient(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("ces", region)
}

func (c *Config) ltsV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("lts", region)
}

func (c *Config) SmnV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("smn", region)
}

// ********** client for Security **********
func (c *Config) antiddosV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("anti-ddos", region)
}

func (c *Config) kmsKeyV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("kms", region)
}

// ********** client for Enterprise Intelligence **********
func (c *Config) MrsV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("mrs", region)
}

func (c *Config) dwsV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("dws", region)
}

func (c *Config) dliV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("dli", region)
}

func (c *Config) disV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("disv2", region)
}

func (c *Config) cssV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("css", region)
}

func (c *Config) cloudStreamV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("cs", region)
}

func (c *Config) cloudtableV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("cloudtable", region)
}

func (c *Config) cdmV11Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("cdmv11", region)
}

func (c *Config) gesV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("ges", region)
}

// ********** client for Application **********
func (c *Config) apiGatewayV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("apig", region)
}

func (c *Config) dcsV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("dcsv1", region)
}

func (c *Config) dcsV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("dcsv2", region)
}

func (c *Config) dmsV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("dms", region)
}

func (c *Config) dmsV2Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("dmsv2", region)
}

// ********** client for Database **********
func (c *Config) RdsV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("rdsv1", region)
}

func (c *Config) RdsV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("rdsv3", region)
}

func (c *Config) ddsV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("ddsv3", region)
}

func (c *Config) GeminiDBV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("cassandra", region)
}

func (c *Config) openGaussV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("opengauss", region)
}

func (c *Config) gaussdbV3Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("gaussdb", region)
}

// ********** client for Others **********
func (c *Config) BssV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("bss", region)
}

func (c *Config) maasV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("oms", region)
}

func (c *Config) orchestrationV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("rts", region)
}

func (c *Config) mlsV1Client(region string) (*golangsdk.ServiceClient, error) {
	return c.NewServiceClient("mls", region)
}
