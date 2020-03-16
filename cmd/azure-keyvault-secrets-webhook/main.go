// Copyright © 2019 Sparebanken Vest
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Note: Code is based on bank-vaults from Banzai Cloud
//       (https://github.com/banzaicloud/bank-vaults)

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	whhttp "github.com/slok/kubewebhook/pkg/http"
	internalLog "github.com/slok/kubewebhook/pkg/log"
	"github.com/slok/kubewebhook/pkg/observability/metrics"
	whcontext "github.com/slok/kubewebhook/pkg/webhook/context"
	"github.com/slok/kubewebhook/pkg/webhook/mutating"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/azure/auth"

	dockerref "github.com/docker/distribution/reference"
	dockertypes "github.com/docker/docker/api/types"
	dockerclient "github.com/docker/docker/client"
)

const (
	dockerHubHost      = "index.docker.io"
	oldDockerHubHost   = "docker.io"
	injectorDir        = "/azure-keyvault/"
	injectorExecutable = "azure-keyvault-env"
	clientCertDir      = "/client-cert/"
)

type azureKeyVaultConfig struct {
	customAuth               bool
	customAuthAutoInject     bool
	credentials              *AzureKeyVaultCredentials
	credentialsSecretName    string
	namespace                string
	aadPodBindingLabel       string
	cloudConfigHostPath      string
	cloudConfigContainerPath string
	dockerPullTimeout        int
	serveMetrics             bool
	metricsAddress           string
	certFile                 string
	keyFile                  string
	caFile                   string
	clientCertFile           string
	clientKeyFile            string
	clientCertSecretName     string
	webhookAuthServiceName   string
	webhookAuthServicePort   string
}

var config azureKeyVaultConfig

var (
	podsMutatedCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "akv2k8s_pod_mutations_total",
		Help: "The total number of pods mutated",
	})
)

var (
	podsInspectedCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "akv2k8s_pod_inspections_total",
		Help: "The total number of pods inspected, including mutated",
	})
)

var (
	podsMutatedFailedCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "akv2k8s_pod_mutations_failed_total",
		Help: "The total number of attempted pod mutations that failed",
	})
)

const envVarReplacementKey = "@azurekeyvault"

func setLogLevel(logLevel string) {
	if logLevel == "" {
		logLevel = log.InfoLevel.String()
	}

	logrusLevel, err := log.ParseLevel(logLevel)
	if err != nil {
		log.Fatalf("error setting log level: %s", err.Error())
	}
	log.SetLevel(logrusLevel)
}

func useClientCert() bool {
	return !config.customAuth || (config.customAuth && !config.customAuthAutoInject)
}

// This init-container copies a program to /azure-keyvault/ and
// if default auth copies a read only version of azure config into
// the /azure-keyvault/ folder to use as auth
func getInitContainers() []corev1.Container {
	fullExecPath := filepath.Join(injectorDir, injectorExecutable)
	cmd := fmt.Sprintf("echo 'Copying %s to %s'", fullExecPath, injectorDir)
	// cmd = cmd + fmt.Sprintf(" && chmod 777 %s", injectorDir)
	cmd = cmd + fmt.Sprintf(" && cp /usr/local/bin/%s %s", injectorExecutable, injectorDir)
	// cmd = cmd + fmt.Sprintf(" && chmod 777 %s", fullExecPath)

	// if !config.customAuth {
	// 	cmd = cmd + fmt.Sprintf(" && cp %s %s", config.cloudConfigHostPath, config.cloudConfigContainerPath)
	// 	cmd = cmd + fmt.Sprintf(" && chmod 666 %s", config.cloudConfigContainerPath)
	// }

	container := corev1.Container{
		Name:            "copy-azurekeyvault-env",
		Image:           viper.GetString("azurekeyvault_env_image"),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"sh", "-c", cmd},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "azure-keyvault-env",
				MountPath: injectorDir,
			},
		},
	}

	// if !config.customAuth {
	// 	container.VolumeMounts = append(container.VolumeMounts, []corev1.VolumeMount{
	// 		{
	// 			Name:      "azure-config",
	// 			MountPath: config.cloudConfigHostPath,
	// 			ReadOnly:  true,
	// 		},
	// 	}...)
	// }

	return []corev1.Container{container}
}

func getVolumes() []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "azure-keyvault-env",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory,
				},
			},
		},
	}

	// if !config.customAuth {
	// 	hostPathFile := corev1.HostPathFile

	// 	volumes = append(volumes, []corev1.Volume{
	// 		{
	// 			Name: "azure-config",
	// 			VolumeSource: corev1.VolumeSource{
	// 				HostPath: &corev1.HostPathVolumeSource{
	// 					Path: config.cloudConfigHostPath,
	// 					Type: &hostPathFile,
	// 				},
	// 			},
	// 		},
	// 	}...)
	// }

	if useClientCert() {
		volumes = append(volumes, []corev1.Volume{
			{
				Name: "client-cert",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: config.clientCertSecretName,
					},
				},
			},
		}...)
	}

	return volumes
}

func namespace() string {
	if ns, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		return ns
	}

	if data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}

	return "default"
}

func vaultSecretsMutator(ctx context.Context, obj metav1.Object) (bool, error) {
	req := whcontext.GetAdmissionRequest(ctx)
	config.namespace = req.Namespace
	var pod *corev1.Pod

	switch v := obj.(type) {
	case *corev1.Pod:
		log.Infof("found pod to mutate in namespace '%s'", config.namespace)
		pod = v
	default:
		return false, nil
	}

	podsInspectedCounter.Inc()
	err := mutatePodSpec(pod)

	if err != nil {
		podsMutatedFailedCounter.Inc()
	}

	return false, err
}

func mutateContainers(containers []corev1.Container, creds map[string]string) (bool, error) {
	mutated := false
	for i, container := range containers {
		log.Infof("found container '%s' to mutate", container.Name)

		var envVars []corev1.EnvVar
		log.Infof("checking for env vars containing '%s' in container %s", envVarReplacementKey, container.Name)
		for _, env := range container.Env {
			if strings.Contains(env.Value, envVarReplacementKey) {
				log.Infof("found env var: %s", env.Value)
				envVars = append(envVars, env)
			}
		}
		if len(envVars) == 0 {
			log.Info("found no env vars in container")
			continue
		}

		registryName := ""
		imgParts := strings.Split(container.Image, "/")
		if len(imgParts) >= 2 {
			registryName = imgParts[0]
		}

		regCred, ok := creds[registryName]

		if ok {
			log.Infof("found credentials to use with registry '%s'", registryName)
		} else {
			log.Infof("did not find credentials to use with registry '%s' - getting default credentials", registryName)
			regCred, ok = getAcrCreds(registryName)
		}

		autoArgs, err := getContainerCmd(container, regCred)
		if err != nil {
			return false, fmt.Errorf("failed to get auto cmd, error: %+v", err)
		}

		log.Infof("using '%s' as arguments for env-injector", strings.Join(autoArgs, " "))

		mutated = true

		fullExecPath := filepath.Join(injectorDir, injectorExecutable)
		container.Command = []string{fullExecPath}
		container.Args = autoArgs

		container.VolumeMounts = append(container.VolumeMounts, []corev1.VolumeMount{
			{
				Name:      "azure-keyvault-env",
				MountPath: injectorDir,
			},
		}...)

		if useClientCert() {
			container.VolumeMounts = append(container.VolumeMounts, []corev1.VolumeMount{
				{
					Name:      "client-cert",
					MountPath: clientCertDir,
				},
			}...)
		}

		container.Env = append(container.Env, []corev1.EnvVar{
			{
				Name: "ENV_INJECTOR_POD_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
			{
				Name:  "ENV_INJECTOR_CUSTOM_AUTH",
				Value: strconv.FormatBool(config.customAuth),
			},
			{
				Name:  "ENV_INJECTOR_HAS_CLIENT_CERT",
				Value: strconv.FormatBool(useClientCert()),
			},
			{
				Name:  "ENV_INJECTOR_AUTH_SERVICE",
				Value: fmt.Sprintf("%s.%s.svc.cluster.local:%s", config.webhookAuthServiceName, namespace(), config.webhookAuthServicePort),
			},
		}...)

		if config.customAuth && config.customAuthAutoInject && config.credentials.CredentialsType != CredentialsTypeManagedIdentitiesForAzureResources {
			container.Env = append(container.Env, *config.credentials.GetEnvVarFromSecret(config.credentialsSecretName)...)
		}

		containers[i] = container
	}

	return mutated, nil
}

func getContainerCmd(container corev1.Container, creds string) ([]string, error) {
	var image *dockertypes.ImageInspect
	var err error

	cmd := container.Command

	// If container.Command is set it will override both image.Entrypoint AND image.Cmd
	// https://kubernetes.io/docs/tasks/inject-data-application/define-command-argument-container/#notes
	if len(cmd) == 0 {
		image, err = getDockerImage(container, creds)
		if err != nil {
			return nil, err
		}

		if image == nil {
			return nil, fmt.Errorf("when getting docker image description for %s, an empty description was returned", container.Image)
		}

		cmd = append(cmd, image.Config.Entrypoint...)

		if len(container.Args) == 0 {
			cmd = append(cmd, image.Config.Cmd...)
		}
	}

	cmd = append(cmd, container.Args...)

	return cmd, nil
}

func getDockerImage(container corev1.Container, creds string) (*dockertypes.ImageInspect, error) {
	timeout := time.Duration(config.dockerPullTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	opt := dockertypes.ImagePullOptions{
		RegistryAuth: creds,
	}

	cli, err := dockerclient.NewEnvClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client, error: %+v", err)
	}

	imageTag := "latest"
	imageParts := strings.Split(container.Image, ":")
	if len(imageParts) > 1 {
		imageTag = imageParts[1]
	}

	named, err := dockerref.ParseNormalizedNamed(container.Image)
	if err != nil {
		return nil, fmt.Errorf("failed to parse image name, error: %+v", err)
	}

	imageName := named.Name() + ":" + imageTag

	// pull image in case its not present on host yet
	log.Infof("pulling docker image %s to get entrypoint and cmd, timeout is %d seconds", imageName, timeout/time.Second)
	imgReader, err := cli.ImagePull(ctx, imageName, opt)

	if err != nil {
		return nil, fmt.Errorf("failed to pull docker image '%s', error: %+v", imageName, err)
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	imgPullOutput, err := ioutil.ReadAll(imgReader)
	log.Debugf("docker pull image output: %s", imgPullOutput)
	// io.Copy(os.Stdout, imgReader)

	log.Infof("docker image %s pulled successfully", imageName)
	defer imgReader.Close()

	log.Infof("inspecting container image %s, looking for entrypoint and cmd", imageName)
	inspect, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect docker image '%s', error: %+v", imageName, err)
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return &inspect, nil
}

func getRegistryCreds(clientset kubernetes.Clientset, podSpec *corev1.PodSpec) (map[string]string, error) {
	creds := make(map[string]string)

	var conf struct {
		Auths map[string]struct {
			Auth string
		}
	}

	var decoded []byte
	var ok bool
	if podSpec.ImagePullSecrets != nil {
		for _, secret := range podSpec.ImagePullSecrets {
			secret, err := clientset.CoreV1().Secrets(config.namespace).Get(secret.Name, metav1.GetOptions{})
			if err != nil {
				return creds, err
			}

			switch secret.Type {
			case corev1.SecretTypeDockerConfigJson:
				decoded, ok = secret.Data[corev1.DockerConfigJsonKey]
			default:
				return creds, fmt.Errorf("unable to load image pull secret '%s', only type '%s' is supported", secret.Name, secret.Type)
			}

			if !ok {
				continue
			}

			if err := json.Unmarshal(decoded, &conf); err != nil {
				return creds, err
			}

			// If it's in k8s format, it won't have the surrounding "Auth". Try that too.
			if len(conf.Auths) == 0 {
				if err := json.Unmarshal(decoded, &conf.Auths); err != nil {
					return creds, err
				}
			}

			for host, entry := range conf.Auths {
				decodedAuth, err := base64.StdEncoding.DecodeString(entry.Auth)
				if err != nil {
					return creds, err
				}

				authParts := strings.SplitN(string(decodedAuth), ":", 2)
				if len(authParts) != 2 {
					return creds, fmt.Errorf("decoded credential has wrong number of fields (expected 2, got %d)", len(authParts))
				}

				credsValue := dockertypes.AuthConfig{
					Username: authParts[0],
					Password: authParts[1],
				}
				encodedJSON, err := json.Marshal(credsValue)
				if err != nil {
					return creds, err
				}

				creds[host] = base64.URLEncoding.EncodeToString(encodedJSON)
			}
		}
	}
	return creds, nil
}

func getAcrCreds(host string) (string, bool) {
	if !hostIsAzureContainerRegistry(host) {
		log.Infof("registry host '%s' is not a acr registry", host)
		return "", false
	}

	bytes, err := ioutil.ReadFile(config.cloudConfigHostPath)
	if err != nil {
		log.Infof("failed to read azure.json to get default credentials, error: %v", err)
		return "", false //creds, fmt.Errorf("failed to read cloud config file in an effort to get credentials for azure key vault, error: %+v", err)
	}

	azureConfig := auth.AzureAuthConfig{}
	if err = yaml.Unmarshal(bytes, &azureConfig); err != nil {
		log.Infof("failed to unmarshall azure config, error: %v", err)
		return "", false // creds, fmt.Errorf("Unmarshall error: %v", err)
	}

	var credsValue dockertypes.AuthConfig
	if azureConfig.AADClientID != "" {
		log.Infof("using default credentials for docker registry with clientid: %s", azureConfig.AADClientID)
		credsValue = dockertypes.AuthConfig{
			Username: azureConfig.AADClientID,
			Password: azureConfig.AADClientSecret,
		}
	} else {
		log.Info("aadclientid is not set i azure config, so have no credentials to use")
		return "", false // nil, fmt.Errorf("Failed to find credentials for docker registry '%s'", regHost)
	}

	encodedJSON, err := json.Marshal(credsValue)
	if err != nil {
		log.Errorf("failed to marshall credentials, error: %v\n", err)
		return "", false // creds, err
	}
	return base64.URLEncoding.EncodeToString(encodedJSON), true
}

func hostIsAzureContainerRegistry(host string) bool {
	for _, v := range []string{".azurecr.io", ".azurecr.cn", ".azurecr.de", ".azurecr.us"} {
		if strings.HasSuffix(host, v) {
			return true
		}
	}
	return false
}

func mutatePodSpec(pod *corev1.Pod) error {
	podSpec := &pod.Spec

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	regCred, err := getRegistryCreds(*clientset, podSpec)
	if err != nil {
		return err
	}

	initContainersMutated, err := mutateContainers(podSpec.InitContainers, regCred)
	if err != nil {
		return err
	}

	containersMutated, err := mutateContainers(podSpec.Containers, regCred)
	if err != nil {
		return err
	}

	if initContainersMutated || containersMutated {
		if config.namespace != "" && config.customAuth && config.customAuthAutoInject {
			if config.credentials.CredentialsType == CredentialsTypeManagedIdentitiesForAzureResources {
				if pod.Labels == nil {
					pod.Labels = make(map[string]string)
					pod.Labels["aadpodidbinding"] = config.aadPodBindingLabel
				}
			} else {
				log.Infof("creating secret in new namespace '%s'...", config.namespace)

				keyVaultSecret, err := config.credentials.GetKubernetesSecret(config.credentialsSecretName)
				if err != nil {
					return err
				}

				_, err = clientset.CoreV1().Secrets(config.namespace).Create(keyVaultSecret)
				if err != nil {
					if errors.IsAlreadyExists(err) {
						_, err = clientset.CoreV1().Secrets(config.namespace).Update(keyVaultSecret)
						if err != nil {
							return err
						}
					} else {
						return err
					}
				}
			}
		}

		if config.namespace != "" && useClientCert() {
			log.Infof("creating client cert secret in new namespace '%s'...", config.namespace)

			clientCertSecret, err := createClientCertSecret(config.clientCertSecretName)
			if err != nil {
				return err
			}

			_, err = clientset.CoreV1().Secrets(config.namespace).Create(clientCertSecret)
			if err != nil {
				if errors.IsAlreadyExists(err) {
					_, err = clientset.CoreV1().Secrets(config.namespace).Update(clientCertSecret)
					if err != nil {
						return err
					}
				} else {
					return err
				}
			}
		}

		podSpec.InitContainers = append(getInitContainers(), podSpec.InitContainers...)
		podSpec.Volumes = append(podSpec.Volumes, getVolumes()...)
		log.Info("containers mutated and pod updated with init-container and volumes")
		podsMutatedCounter.Inc()
	} else {
		log.Info("no containers mutated")
	}

	return nil
}

func createClientCertSecret(secretName string) (*corev1.Secret, error) {
	clientCert, err := ioutil.ReadFile(config.clientCertFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read client cert file from %s, error: %+v", config.clientCertFile, err)
	}

	clientKey, err := ioutil.ReadFile(config.clientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read client key file from %s, error: %+v", config.clientKeyFile, err)
	}

	caCert, err := ioutil.ReadFile(config.caFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read ca cert file from %s, error: %+v", config.caFile, err)
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName,
		},
		StringData: map[string]string{
			"clientCert": string(clientCert),
			"clientKey":  string(clientKey),
			"caCert":     string(caCert),
		},
	}, nil
}

func initConfig() {
	viper.SetDefault("azurekeyvault_env_image", "spvest/azure-keyvault-env:latest")
	viper.SetDefault("custom_docker_pull_timeout", 120)
	viper.SetDefault("custom_auth_inject_secret_name", "akv2k8s-akv-credentials")
	viper.SetDefault("client_cert_secret_name", "akv2k8s-client-cert")
	viper.AutomaticEnv()
}

func handlerFor(config mutating.WebhookConfig, mutator mutating.MutatorFunc, recorder metrics.Recorder, logger internalLog.Logger) http.Handler {
	webhook, err := mutating.NewWebhook(config, mutator, nil, nil, logger)
	if err != nil {
		log.Errorf("error creating webhook: %s", err)
		os.Exit(1)
	}

	handler, err := whhttp.HandlerFor(webhook)
	if err != nil {
		log.Errorf("error creating webhook: %s", err)
		os.Exit(1)
	}

	return handler
}

// accept a client certificate for authentication (which is be provided by init-container)
func authHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		token, err := config.credentials.GetAzureToken()
		if err != nil {
			fmt.Fprintf(w, "failed to get azure token: %s", err.Error())
			w.WriteHeader(http.StatusNotFound)
			return
		}

		fmt.Fprint(w, token)
		w.WriteHeader(http.StatusOK)
	} else {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
	}
}

func main() {
	fmt.Fprintln(os.Stdout, "initializing config...")
	initConfig()
	fmt.Fprintln(os.Stdout, "config initialized")

	logLevel := viper.GetString("LOG_LEVEL")
	logger := &internalLog.Std{Debug: logLevel == "debug" || logLevel == "trace"}

	setLogLevel(logLevel)

	config = azureKeyVaultConfig{
		customAuth:               viper.GetBool("CUSTOM_AUTH"),
		customAuthAutoInject:     viper.GetBool("CUSTOM_AUTH_INJECT"),
		credentialsSecretName:    viper.GetString("CUSTOM_AUTH_INJECT_SECRET_NAME"),
		dockerPullTimeout:        viper.GetInt("CUSTOM_DOCKER_PULL_TIMEOUT"),
		cloudConfigHostPath:      "/etc/kubernetes/azure.json",
		cloudConfigContainerPath: "/azure-keyvault/azure.json",
		serveMetrics:             viper.GetBool("METRICS_ENABLED"),
		metricsAddress:           viper.GetString("METRICS_ADDR"),
		certFile:                 viper.GetString("tls_cert_file"),
		keyFile:                  viper.GetString("tls_private_key_file"),
		caFile:                   viper.GetString("tls_ca_file"),
		clientCertFile:           viper.GetString("tls_client_file"),
		clientKeyFile:            viper.GetString("tls_client_key_file"),
		clientCertSecretName:     viper.GetString("client_cert_secret_name"),
		webhookAuthServiceName:   viper.GetString("webhook_auth_service"),
		webhookAuthServicePort:   viper.GetString("webhook_auth_service_port"),
	}

	if config.customAuth {
		azureCreds, err := NewCredentials()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error getting credentials: %s", err)
			os.Exit(1)
		}

		config.credentials = azureCreds

		if azureCreds.CredentialsType == CredentialsTypeManagedIdentitiesForAzureResources {
			config.aadPodBindingLabel = viper.GetString("aad_pod_binding_label")
		}
	}

	if config.metricsAddress == "" {
		config.metricsAddress = ":80"
	}

	mutator := mutating.MutatorFunc(vaultSecretsMutator)
	metricsRecorder := metrics.NewPrometheus(prometheus.DefaultRegisterer)

	podHandler := handlerFor(mutating.WebhookConfig{Name: "azurekeyvault-secrets-pods", Obj: &corev1.Pod{}}, mutator, metricsRecorder, logger)

	if config.serveMetrics {
		go func() {
			log.Infof("Metrics at http://%s", config.metricsAddress)

			metricMux := http.NewServeMux()
			metricMux.Handle("/metrics", promhttp.Handler())
			err := http.ListenAndServe(config.metricsAddress, metricMux)
			if err != nil {
				log.Fatalf("error serving metrics: %s", err)
			}
		}()
	}

	go func() {
		caCert, err := ioutil.ReadFile(config.caFile)
		if err != nil {
			log.Fatal(err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig := &tls.Config{
			ClientCAs:  caCertPool,
			ClientAuth: tls.RequireAndVerifyClientCert,
		}
		tlsConfig.BuildNameToCertificate()

		authMux := http.NewServeMux()
		authMux.HandleFunc("/auth", authHandler)

		authServer := &http.Server{
			Addr:      ":8443",
			TLSConfig: tlsConfig,
			Handler:   authMux,
		}

		logger.Infof("auth listening on :8443")
		err = authServer.ListenAndServeTLS(config.certFile, config.keyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error serving webhook auth endpoint: %s", err)
			os.Exit(1)
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/pods", podHandler)

	logger.Infof("listening on :443")
	err := http.ListenAndServeTLS(":443", config.certFile, config.keyFile, mux)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error serving webhook: %s", err)
		os.Exit(1)
	}
}
