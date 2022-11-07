package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/armon/circbuf"
	dkplugin "github.com/distribworks/dkron/v3/plugin"
	dktypes "github.com/distribworks/dkron/v3/plugin/types"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerinstance/armcontainerinstance"
)

const (
	// maxBufSize limits how much data we collect from a handler.
	// This is to prevent Serf's memory from growing to an enormous
	// amount due to a faulty handler.
	maxBufSize = 256000
)

// reportingWriter This is a Writer implementation that writes back to the host
type reportingWriter struct {
	buffer  *circbuf.Buffer
	cb      dkplugin.StatusHelper
	isError bool
}

func (p reportingWriter) Write(data []byte) (n int, err error) {
	p.cb.Update(data, p.isError)
	return p.buffer.Write(data)
}

// Shell plugin runs shell commands when Execute method is called.
type AzureContainerInstance struct{}

// Execute method of the plugin
func (aci *AzureContainerInstance) Execute(args *dktypes.ExecuteRequest, cb dkplugin.StatusHelper) (*dktypes.ExecuteResponse, error) {
	out, err := aci.ExecuteImpl(args, cb)
	resp := &dktypes.ExecuteResponse{Output: out}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp, nil
}

// ExecuteImpl do execute command
func (aci *AzureContainerInstance) ExecuteImpl(args *dktypes.ExecuteRequest, cb dkplugin.StatusHelper) ([]byte, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}

	cfg, err := getAciConfig(args.Config)
	if err != nil {
		return nil, err
	}

	client, err := armcontainerinstance.NewContainerGroupsClient(cfg.subscriptionId, cred, &arm.ClientOptions{})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	containerGroup := armcontainerinstance.ContainerGroup{
		Name:     &cfg.containerGroupName,
		Location: &cfg.location,
		Properties: &armcontainerinstance.ContainerGroupProperties{
			OSType:        &cfg.osType,
			RestartPolicy: &cfg.restartPolicy,
			Containers: []*armcontainerinstance.Container{
				{
					Name: &cfg.containerGroupName,
					Properties: &armcontainerinstance.ContainerProperties{
						Image:   &cfg.containerImage,
						Command: convertStringSliceToAzureSDK(cfg.command),
						Resources: &armcontainerinstance.ResourceRequirements{
							Requests: &armcontainerinstance.ResourceRequests{
								CPU:        &cfg.requestCPU,
								MemoryInGB: &cfg.requestMemoryInGB,
							},
						},
					},
				},
			},
		},
	}
	poller, err := client.BeginCreateOrUpdate(ctx, cfg.resourceGroupName, cfg.containerGroupName, containerGroup, &armcontainerinstance.ContainerGroupsClientBeginCreateOrUpdateOptions{})
	if err != nil {
		return nil, err
	}

	res, err := poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{})
	if err != nil {
		return nil, err
	}

	if res.ID == nil {
		return nil, fmt.Errorf("response id is nil")
	}

	defer func() {
		poller, err := client.BeginDelete(context.Background(), cfg.resourceGroupName, cfg.containerGroupName, &armcontainerinstance.ContainerGroupsClientBeginDeleteOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Unable to delete %s: %v", cfg.containerGroupName, err)
			return
		}
		_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Unable to delete %s: %v", cfg.containerGroupName, err)
			return
		}
	}()

	getCtx, getCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer getCancel()

	success := false
	var getErr error
	for i := 0; i < 10; i++ {
		var getRes armcontainerinstance.ContainerGroupsClientGetResponse
		getRes, getErr = client.Get(getCtx, cfg.resourceGroupName, cfg.containerGroupName, &armcontainerinstance.ContainerGroupsClientGetOptions{})
		if getRes.Properties != nil && getRes.Properties.ProvisioningState != nil && *getRes.Properties.ProvisioningState == "Succeeded" {
			success = true
		}
		time.Sleep(10 * time.Second)
	}

	if !success || getErr != nil {
		return nil, getErr
	}

	logClient, err := armcontainerinstance.NewContainersClient(cfg.subscriptionId, cred, &arm.ClientOptions{})
	if err != nil {
		return nil, err
	}

	logCtx, logCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer logCancel()

	logs, err := logClient.ListLogs(logCtx, cfg.resourceGroupName, cfg.containerGroupName, cfg.containerGroupName, &armcontainerinstance.ContainersClientListLogsOptions{})
	if err != nil {
		return nil, err
	}

	if logs.Content == nil {
		return nil, fmt.Errorf("log content is nil")
	}

	return []byte(fmt.Sprintf("%s logs:\n%s", *res.ID, *logs.Content)), nil
}

type aciConfig struct {
	subscriptionId     string
	resourceGroupName  string
	containerGroupName string
	containerImage     string
	location           string
	osType             armcontainerinstance.OperatingSystemTypes
	requestCPU         float64
	requestMemoryInGB  float64
	command            []string
	restartPolicy      armcontainerinstance.ContainerGroupRestartPolicy
}

func getAciConfig(config map[string]string) (aciConfig, error) {
	subscriptionId, err := getConfigParameter(config, "subscription_id")
	if err != nil {
		return aciConfig{}, err
	}

	resourceGroupName, err := getConfigParameter(config, "resource_group_name")
	if err != nil {
		return aciConfig{}, err
	}

	containerGroupName, err := getConfigParameter(config, "container_group_name")
	if err != nil {
		return aciConfig{}, err
	}

	containerImage, err := getConfigParameter(config, "container_image")
	if err != nil {
		return aciConfig{}, err
	}

	location, err := getConfigParameter(config, "location")
	if err != nil {
		return aciConfig{}, err
	}

	osTypeStr, err := getConfigParameter(config, "os_type")
	if err != nil {
		return aciConfig{}, err
	}

	var osType armcontainerinstance.OperatingSystemTypes
	switch osTypeStr {
	case string(armcontainerinstance.OperatingSystemTypesLinux):
		osType = armcontainerinstance.OperatingSystemTypesLinux
	case string(armcontainerinstance.OperatingSystemTypesWindows):
		osType = armcontainerinstance.OperatingSystemTypesLinux
	default:
		return aciConfig{}, fmt.Errorf("os_type %q not supported, use either Linux or Windows", osTypeStr)
	}

	requestCPU, err := getConfigFloat64Parameter(config, "request_cpu")
	if err != nil {
		return aciConfig{}, err
	}

	requestMemoryInGB, err := getConfigFloat64Parameter(config, "request_memory_in_gb")
	if err != nil {
		return aciConfig{}, err
	}

	command, err := getConfigStringSliceParameter(config, "command")
	if err != nil {
		return aciConfig{}, err
	}

	restartPolicyStr, err := getConfigParameter(config, "restart_policy")
	if err != nil {
		return aciConfig{}, err
	}

	var restartPolicy armcontainerinstance.ContainerGroupRestartPolicy
	switch restartPolicyStr {
	case string(armcontainerinstance.ContainerGroupRestartPolicyAlways):
		restartPolicy = armcontainerinstance.ContainerGroupRestartPolicyAlways
	case string(armcontainerinstance.ContainerGroupRestartPolicyNever):
		restartPolicy = armcontainerinstance.ContainerGroupRestartPolicyNever
	case string(armcontainerinstance.ContainerGroupRestartPolicyOnFailure):
		restartPolicy = armcontainerinstance.ContainerGroupRestartPolicyOnFailure
	default:
		return aciConfig{}, fmt.Errorf("restart_policy %q not supported, use Always, Never or OnFailure", osTypeStr)
	}

	return aciConfig{
		subscriptionId,
		resourceGroupName,
		containerGroupName,
		containerImage,
		location,
		osType,
		requestCPU,
		requestMemoryInGB,
		command,
		restartPolicy,
	}, nil
}

func getConfigParameter(config map[string]string, name string) (string, error) {
	param, ok := config[name]
	if !ok {
		return "", fmt.Errorf("unable to find %q in args", name)
	}
	return param, nil
}

func getConfigStringSliceParameter(config map[string]string, name string) ([]string, error) {
	param, err := getConfigParameter(config, name)
	if err != nil {
		return nil, err
	}

	var s []string
	err = json.Unmarshal([]byte(param), &s)
	if err != nil {
		return nil, err
	}

	return s, nil
}

func getConfigFloat64Parameter(config map[string]string, name string) (float64, error) {
	param, err := getConfigParameter(config, name)
	if err != nil {
		return 0, err
	}

	f, err := strconv.ParseFloat(param, 64)
	if err != nil {
		return 0, err
	}

	return f, nil
}

func convertStringSliceToAzureSDK(s []string) []*string {
	var ss []*string
	for _, v := range s {
		vv := v
		ss = append(ss, &vv)
	}
	return ss
}
