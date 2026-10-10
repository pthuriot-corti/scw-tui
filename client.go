package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/scaleway/scaleway-sdk-go/api/account/v3"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	"github.com/scaleway/scaleway-sdk-go/api/lb/v1"
	"github.com/scaleway/scaleway-sdk-go/api/vpcgw/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

var ctx = context.Background()

// Clients holds all SDK API clients used by the TUI.
type Clients struct {
	scw                *scw.Client
	account            *account.ProjectAPI
	instance           *instance.API
	lb                 *lb.API
	vpcgw              *vpcgw.API
	s3                 *s3sdk.Client
	region             scw.Region
	defaultProjectID   string
	defaultProjectName string
}

// newClients creates all API clients from the Scaleway config file + env.
func newClients() (*Clients, error) {
	config, err := scw.LoadConfig()
	if err != nil {
		// fall back to env-only client
		return newClientsFromEnv()
	}

	profile, err := config.GetActiveProfile()
	if err != nil {
		return nil, fmt.Errorf("get active profile: %w", err)
	}

	client, err := scw.NewClient(scw.WithProfile(profile))
	if err != nil {
		return nil, fmt.Errorf("create scw client: %w", err)
	}

	c, err := buildClients(client, profile)
	if err != nil {
		return nil, err
	}
	c.resolveDefaultProject()
	return c, nil
}

func newClientsFromEnv() (*Clients, error) {
	client, err := scw.NewClient()
	if err != nil {
		return nil, fmt.Errorf("create scw client from env: %w", err)
	}
	c, err := buildClients(client, nil)
	if err != nil {
		return nil, err
	}
	c.resolveDefaultProject()
	return c, nil
}

func buildClients(client *scw.Client, profile *scw.Profile) (*Clients, error) {
	c := &Clients{
		scw:      client,
		account:  account.NewProjectAPI(client),
		instance: instance.NewAPI(client),
		lb:       lb.NewAPI(client),
		vpcgw:    vpcgw.NewAPI(client),
	}

	// Determine region for S3 endpoint.
	region := scw.RegionFrPar
	if profile != nil && profile.DefaultRegion != nil && *profile.DefaultRegion != "" {
		region = scw.Region(*profile.DefaultRegion)
	}
	c.region = region

	// Build S3 client using Scaleway credentials.
	s3Endpoint := fmt.Sprintf("https://s3.%s.scw.cloud", region)
	if profile != nil && profile.S3Endpoint != nil && *profile.S3Endpoint != "" {
		s3Endpoint = *profile.S3Endpoint
	}

	var accessKey, secretKey string
	if profile != nil {
		if profile.AccessKey != nil {
			accessKey = *profile.AccessKey
		}
		if profile.SecretKey != nil {
			secretKey = *profile.SecretKey
		}
	}

	awsCfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion(string(region)),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	c.s3 = s3sdk.NewFromConfig(awsCfg, func(o *s3sdk.Options) {
		o.BaseEndpoint = aws.String(s3Endpoint)
		o.UsePathStyle = true
	})
	return c, nil
}

// resolveDefaultProject sets the default project ID and resolves its name.
func (c *Clients) resolveDefaultProject() {
	if pid, exists := c.scw.GetDefaultProjectID(); exists {
		c.defaultProjectID = pid
		// Try to resolve project name via the API.
		resp, err := c.account.ListProjects(&account.ProjectAPIListProjectsRequest{
			ProjectIDs: []string{pid},
		}, scw.WithAllPages())
		if err == nil && len(resp.Projects) > 0 {
			c.defaultProjectName = resp.Projects[0].Name
		} else {
			c.defaultProjectName = pid // fallback to ID as display name
		}
	}
}
