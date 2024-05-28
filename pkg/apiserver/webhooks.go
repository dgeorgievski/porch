// Copyright 2022,2024 The kpt and Nephio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiserver

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nephio-project/porch/api/porch/v1alpha1"
	"github.com/nephio-project/porch/internal/kpt/util/porch"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	webhookServicePort int32 = 8443
	serverEndpoint           = "/validate-deletion"
)

type WebhookType string

const (
	WebhookTypeService WebhookType = "service"
	WebhookTypeUrl     WebhookType = "url"
)

// WebhookConfig defines the configuration for the PackageRevision deletion webhook
type WebhookConfig struct {
	Type             WebhookType
	ServiceName      string // only used if Type == WebhookTypeService
	ServiceNamespace string // only used if Type == WebhookTypeService
	Host             string // only used if Type == WebhookTypeUrl
	Path             string
	Port             int32
	CertStorageDir   string
}

func NewWebhookConfig() *WebhookConfig {
	var cfg WebhookConfig
	// NOTE: CERT_NAMESPACE is supported for backward compatibility.
	// TODO: We may consider using only WEBHOOK_SERVICE_NAMESPACE instead.
	if hasEnv("CERT_NAMESPACE") ||
		hasEnv("WEBHOOK_SERVICE_NAME") ||
		hasEnv("WEBHOOK_SERVICE_NAMESPACE") ||
		!hasEnv("WEBHOOK_HOST") {

		cfg.Type = WebhookTypeService
		cfg.ServiceName = getEnv("WEBHOOK_SERVICE_NAME", "api")
		cfg.ServiceNamespace = getEnv("WEBHOOK_SERVICE_NAMESPACE", "porch-system")
		cfg.ServiceNamespace = getEnv("CERT_NAMESPACE", cfg.ServiceNamespace)
		cfg.Host = fmt.Sprintf("%s.%s.svc", cfg.ServiceName, cfg.ServiceNamespace)
	} else {
		cfg.Type = WebhookTypeUrl
		cfg.Host = getEnv("WEBHOOK_HOST", "localhost")
	}
	cfg.Path = serverEndpoint
	cfg.Port = getEnvInt32("WEBHOOK_PORT", webhookServicePort)
	cfg.CertStorageDir = getEnv("CERT_STORAGE_DIR", "/tmp/cert")
	return &cfg
}

func setupWebhooks(ctx context.Context) error {
	cfg := NewWebhookConfig()
	caBytes, err := createCerts(cfg)
	if err != nil {
		return err
	}
	if err := createValidatingWebhook(ctx, cfg, caBytes); err != nil {
		return err
	}
	if err := runWebhookServer(cfg.CertStorageDir); err != nil {
		return err
	}
	return nil
}

func createCerts(cfg *WebhookConfig) ([]byte, error) {
	klog.Infof("creating self-signing TLS cert and key for %q in directory %s", cfg.Host, cfg.CertStorageDir)
	commonName := cfg.Host
	dnsNames := []string{commonName}
	if cfg.Type == WebhookTypeService {
		dnsNames = append(dnsNames, cfg.ServiceName)
		dnsNames = append(dnsNames, fmt.Sprintf("%s.%s", cfg.ServiceName, cfg.ServiceNamespace))
		dnsNames = append(dnsNames, fmt.Sprintf("%s.%s.svc", cfg.ServiceName, cfg.ServiceNamespace))
		dnsNames = append(dnsNames, fmt.Sprintf("%s.%s.svc.cluster.local", cfg.ServiceName, cfg.ServiceNamespace))
	}

	var caPEM, serverCertPEM, serverPrivateKeyPEM *bytes.Buffer
	// CA config
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(2020),
		Subject: pkix.Name{
			Organization: []string{"google.com"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	privateKey, err := rsa.GenerateKey(cryptorand.Reader, 4096)
	if err != nil {
		return nil, err
	}
	caBytes, err := x509.CreateCertificate(cryptorand.Reader, ca, ca, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, err
	}
	caPEM = new(bytes.Buffer)
	_ = pem.Encode(caPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})

	// server cert config
	cert := &x509.Certificate{
		DNSNames:     dnsNames,
		SerialNumber: big.NewInt(1658),
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"google.com"},
		},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		SubjectKeyId: []byte{1, 2, 3, 4, 6},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	serverPrivateKey, err := rsa.GenerateKey(cryptorand.Reader, 4096)
	if err != nil {
		return nil, err
	}
	serverCertBytes, err := x509.CreateCertificate(cryptorand.Reader, cert, ca, &serverPrivateKey.PublicKey, privateKey)
	if err != nil {
		return nil, err
	}
	serverCertPEM = new(bytes.Buffer)
	_ = pem.Encode(serverCertPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serverCertBytes,
	})
	serverPrivateKeyPEM = new(bytes.Buffer)
	_ = pem.Encode(serverPrivateKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(serverPrivateKey),
	})

	err = os.MkdirAll(cfg.CertStorageDir, 0777)
	if err != nil {
		return nil, err
	}
	err = WriteFile(filepath.Join(cfg.CertStorageDir, "tls.crt"), serverCertPEM.Bytes())
	if err != nil {
		return nil, err
	}
	err = WriteFile(filepath.Join(cfg.CertStorageDir, "tls.key"), serverPrivateKeyPEM.Bytes())
	if err != nil {
		return nil, err
	}

	return caPEM.Bytes(), nil
}

// WriteFile writes data in the file at the given path
func WriteFile(filepath string, c []byte) error {
	f, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(c)
	if err != nil {
		return err
	}
	return nil
}

func createValidatingWebhook(ctx context.Context, cfg *WebhookConfig, caCert []byte) error {

	klog.Infof("Creating validating webhook for %s:%d", cfg.Host, cfg.Port)

	kubeConfig := ctrl.GetConfigOrDie()
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return fmt.Errorf("failed to setup kubeClient: %v", err)
	}

	var (
		validationCfgName = "packagerev-deletion-validating-webhook"
		fail              = admissionregistrationv1.Fail
		none              = admissionregistrationv1.SideEffectClassNone
	)

	validateConfig := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: validationCfgName,
		},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: "packagerevdeletion.google.com",
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				CABundle: caCert, // CA bundle created earlier
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{Operations: []admissionregistrationv1.OperationType{
				admissionregistrationv1.Delete},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{"porch.kpt.dev"},
					APIVersions: []string{"v1alpha1"},
					Resources:   []string{"packagerevisions"},
				},
			}},
			AdmissionReviewVersions: []string{"v1", "v1beta1"},
			SideEffects:             &none,
			FailurePolicy:           &fail,
		}},
	}
	switch cfg.Type {
	case WebhookTypeService:
		validateConfig.Webhooks[0].ClientConfig.Service = &admissionregistrationv1.ServiceReference{
			Name:      cfg.ServiceName,
			Namespace: cfg.ServiceNamespace,
			Path:      &cfg.Path,
			Port:      &cfg.Port,
		}
	case WebhookTypeUrl:
		url := fmt.Sprintf("https://%s:%d%s", cfg.Host, cfg.Port, cfg.Path)
		validateConfig.Webhooks[0].ClientConfig.URL = &url
	default:
		return fmt.Errorf("invalid webhook type: %s", cfg.Type)
	}

	if err := kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(ctx, validationCfgName, metav1.DeleteOptions{}); err != nil {
		klog.Error("failed to delete existing webhook: %w", err)
	}

	if _, err := kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(ctx, validateConfig,
		metav1.CreateOptions{}); err != nil {
		klog.Infof("failed to create validating webhook for package revision deletion: %s\n", err.Error())
		return err
	}

	return nil
}

func runWebhookServer(certStorageDir string) error {
	certFile := filepath.Join(certStorageDir, "tls.crt")
	keyFile := filepath.Join(certStorageDir, "tls.key")

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	klog.Infoln("Starting webhook server")
	http.HandleFunc(serverEndpoint, validateDeletion)
	server := http.Server{
		Addr: fmt.Sprintf(":%d", webhookServicePort),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}
	go func() {
		err = server.ListenAndServeTLS("", "")
		if err != nil {
			klog.Errorf("could not start server: %v", err)
		}
	}()
	return err
}

func validateDeletion(w http.ResponseWriter, r *http.Request) {
	klog.Infoln("received request to validate deletion")

	admissionReviewRequest, err := decodeAdmissionReview(r)
	if err != nil {
		errMsg := fmt.Sprintf("error getting admission review from request: %v", err)
		writeErr(errMsg, &w)
		return
	}

	// Verify that we have a PackageRevision resource
	pkgRevGVK := metav1.GroupVersionResource{Group: "porch.kpt.dev", Version: "v1alpha1", Resource: "packagerevisions"}
	if admissionReviewRequest.Request.Resource != pkgRevGVK {
		errMsg := fmt.Sprintf("did not receive PackageRevision, got %s", admissionReviewRequest.Request.Resource.Resource)
		writeErr(errMsg, &w)
		return
	}

	// Get the package revision using the name and namespace from the request.
	porchClient, err := createPorchClient()
	if err != nil {
		errMsg := fmt.Sprintf("could not create porch client: %v", err)
		writeErr(errMsg, &w)
		return
	}
	pr := v1alpha1.PackageRevision{}
	if err := porchClient.Get(context.Background(), client.ObjectKey{
		Namespace: admissionReviewRequest.Request.Namespace,
		Name:      admissionReviewRequest.Request.Name,
	}, &pr); err != nil {
		klog.Errorf("could not get package revision: %s", err.Error())
	}

	admissionResponse := &admissionv1.AdmissionResponse{}
	if pr.Spec.Lifecycle == v1alpha1.PackageRevisionLifecyclePublished {
		admissionResponse.Allowed = false
		admissionResponse.Result = &metav1.Status{
			Status:  "Failure",
			Message: fmt.Sprintf("failed to delete package revision %q: published PackageRevisions must be proposed for deletion by setting spec.lifecycle to 'DeletionProposed' prior to deletion", pr.Name),
			Reason:  "Published PackageRevisions must be proposed for deletion by setting spec.lifecycle to 'DeletionProposed' prior to deletion.",
		}
	} else {
		admissionResponse.Allowed = true
		admissionResponse.Result = &metav1.Status{
			Status:  "Success",
			Message: fmt.Sprintf("Successfully deleted package revision %q", pr.Name),
		}
	}

	resp, err := constructResponse(admissionResponse, admissionReviewRequest)
	if err != nil {
		errMsg := fmt.Sprintf("error constructing response: %v", err)
		writeErr(errMsg, &w)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

func decodeAdmissionReview(r *http.Request) (*admissionv1.AdmissionReview, error) {
	if r.Header.Get("Content-Type") != "application/json" {
		return nil, fmt.Errorf("expected Content-Type 'application/json'")
	}
	var requestData []byte
	if r.Body != nil {
		var err error
		requestData, err = ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
	}
	admissionReviewRequest := &admissionv1.AdmissionReview{}
	deserializer := serializer.NewCodecFactory(runtime.NewScheme()).UniversalDeserializer()
	if _, _, err := deserializer.Decode(requestData, nil, admissionReviewRequest); err != nil {
		return nil, err
	}
	if admissionReviewRequest.Request == nil {
		return nil, fmt.Errorf("admission review request is empty")
	}
	return admissionReviewRequest, nil
}

func constructResponse(response *admissionv1.AdmissionResponse,
	request *admissionv1.AdmissionReview) ([]byte, error) {
	var admissionReviewResponse admissionv1.AdmissionReview
	admissionReviewResponse.Response = response
	admissionReviewResponse.SetGroupVersionKind(request.GroupVersionKind())
	admissionReviewResponse.Response.UID = request.Request.UID

	resp, err := json.Marshal(admissionReviewResponse)
	if err != nil {
		return nil, fmt.Errorf("error marshalling response json: %v", err)
	}
	return resp, nil
}

func createPorchClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		klog.Errorf("could not get config: %s", err.Error())
		return nil, err
	}
	porchClient, err := porch.CreateClient(cfg)
	if err != nil {
		klog.Errorf("could not get porch client: %s", err.Error())
		return nil, err
	}
	return porchClient, nil
}

func writeErr(errMsg string, w *http.ResponseWriter) {
	klog.Errorf(errMsg)
	(*w).WriteHeader(500)
	if _, err := (*w).Write([]byte(errMsg)); err != nil {
		klog.Errorf("could not write error message: %v", err)
	}
}

func hasEnv(key string) bool {
	_, found := os.LookupEnv(key)
	return found
}

func getEnv(key string, defaultValue string) string {
	value, found := os.LookupEnv(key)
	if !found {
		return defaultValue
	}
	return value
}

func getEnvInt32(key string, defaultValue int32) int32 {
	value, found := os.LookupEnv(key)
	if !found {
		return defaultValue
	}
	i64, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		panic("could not parse int32 from environment variable: " + key)
	}
	return int32(i64) // this is safe because of the size parameter of the ParseInt call
}
