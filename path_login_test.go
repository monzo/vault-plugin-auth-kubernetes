package kubeauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/mitchellh/mapstructure"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var (
	testNamespace                       = "default"
	testName                            = "vault-auth"
	testUID                             = "d77f89bc-9055-11e7-a068-0800276d99bf"
	testMockTokenReviewFactory          = mockTokenReviewFactory(testName, testNamespace, testUID)
	testMockServiceAccountReaderFactory = mockServiceAccountReaderFactory(map[string]string{"service_role": "authz"})

	testGlobbedNamespace = "def*"
	testGlobbedName      = "vault-*"

	// Projected ServiceAccount tokens have name "default", and require a
	// different mock token reviewer
	testProjectedName        = "default"
	testProjectedUID         = "77c81ad7-1bea-4d94-9ca5-f5d7f3632331"
	testProjectedMockFactory = mockTokenReviewFactory(testProjectedName, testNamespace, testProjectedUID)

	testDefaultPEMs = []string{testECCert, testRSACert}
)

type testBackendConfig struct {
	pems                          []string
	saName                        string
	saNamespace                   string
	aliasNameSource               string
	customMetadataFromAnnotations bool
}

func defaultTestBackendConfig() *testBackendConfig {
	return &testBackendConfig{
		pems:            testDefaultPEMs,
		saName:          testName,
		saNamespace:     testNamespace,
		aliasNameSource: aliasNameSourceDefault,
	}
}

func setupBackend(t *testing.T, config *testBackendConfig) (logical.Backend, logical.Storage) {
	b, storage := getBackend(t)

	// test no certificate
	data := map[string]interface{}{
		"pem_keys":           config.pems,
		"kubernetes_host":    "host",
		"kubernetes_ca_cert": testCACert,
		"enable_custom_metadata_from_annotations": config.customMetadataFromAnnotations,
	}

	req := &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "config",
		Storage:   storage,
		Data:      data,
	}

	resp, err := b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	data = map[string]interface{}{
		"bound_service_account_names":      config.saName,
		"bound_service_account_namespaces": config.saNamespace,
		"policies":                         "test",
		"period":                           "3s",
		"ttl":                              "1s",
		"num_uses":                         12,
		"max_ttl":                          "5s",
		"alias_name_source":                config.aliasNameSource,
	}

	req = &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "role/plugin-test",
		Storage:   storage,
		Data:      data,
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	b.(*kubeAuthBackend).reviewFactory = testMockTokenReviewFactory
	b.(*kubeAuthBackend).serviceAccountReaderFactory = testMockServiceAccountReaderFactory
	return b, storage
}

func TestLogin(t *testing.T) {
	b, storage := setupBackend(t, defaultTestBackendConfig())

	// Test bad inputs
	data := map[string]interface{}{
		"jwt": jwtData,
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
	}

	resp, err := b.HandleRequest(context.Background(), req)
	if resp == nil || !resp.IsError() {
		t.Fatal("expected error")
	}
	if resp.Error().Error() != "missing role" {
		t.Fatalf("unexpected error: %s", resp.Error())
	}

	data = map[string]interface{}{
		"role": "plugin-test",
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if resp == nil || !resp.IsError() {
		t.Fatal("expected error")
	}
	if resp.Error().Error() != "missing jwt" {
		t.Fatalf("unexpected error: %s", resp.Error())
	}

	// test bad role name
	data = map[string]interface{}{
		"role": "plugin-test-bad",
		"jwt":  jwtData,
	}
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if resp == nil || !resp.IsError() {
		t.Fatal("expected error")
	}
	if resp.Error().Error() != `invalid role name "plugin-test-bad"` {
		t.Fatalf("unexpected error: %s", resp.Error())
	}

	// test bad jwt service account
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtBadServiceAccount,
	}
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "service account name not authorized" {
		t.Fatalf("unexpected error: %s", err)
	}
	requireErrorCode(t, err, http.StatusForbidden)

	// test bad jwt key
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtWithBadSigningKey,
	}
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err == nil {
		t.Fatalf("Expected error")
	} else if !errors.Is(err, logical.ErrPermissionDenied) {
		t.Fatalf("unexpected error: %s", err)
	}

	// test successful login
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test successful login for globbed name
	config := defaultTestBackendConfig()
	config.saName = testGlobbedName
	b, storage = setupBackend(t, config)

	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test successful login for globbed namespace
	config = defaultTestBackendConfig()
	config.saNamespace = testGlobbedNamespace
	b, storage = setupBackend(t, config)

	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}
}

func TestLogin_ContextError(t *testing.T) {
	b, storage := setupBackend(t, defaultTestBackendConfig())

	data := map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := b.HandleRequest(ctx, req)
	if err != context.Canceled {
		t.Fatalf("expected context canceled error, got: %v", err)
	}
}

func TestLogin_ECDSA_PEM(t *testing.T) {
	config := defaultTestBackendConfig()
	b, storage := setupBackend(t, config)

	// test no certificate
	data := map[string]interface{}{
		"pem_keys":           []string{ecdsaKey},
		"kubernetes_host":    "host",
		"kubernetes_ca_cert": testCACert,
	}

	req := &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "config",
		Storage:   storage,
		Data:      data,
	}

	resp, err := b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test successful login
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtECDSASigned,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}
}

func TestLogin_NoPEMs(t *testing.T) {
	config := defaultTestBackendConfig()
	b, storage := setupBackend(t, config)

	// test bad jwt service account
	data := map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtBadServiceAccount,
	}
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err := b.HandleRequest(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "service account name not authorized" {
		t.Fatalf("unexpected error: %s", err)
	}
	requireErrorCode(t, err, http.StatusForbidden)

	// test successful login
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}
}

func TestLoginSvcAcctAndNamespaceSplats(t *testing.T) {
	config := defaultTestBackendConfig()
	config.saName = "*"
	config.saNamespace = "*"
	b, storage := setupBackend(t, config)

	// Test bad inputs
	data := map[string]interface{}{
		"jwt": jwtData,
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
	}

	resp, err := b.HandleRequest(context.Background(), req)
	if resp == nil || !resp.IsError() {
		t.Fatal("expected error")
	}
	if resp.Error().Error() != "missing role" {
		t.Fatalf("unexpected error: %s", resp.Error())
	}

	data = map[string]interface{}{
		"role": "plugin-test",
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if resp == nil || !resp.IsError() {
		t.Fatal("expected error")
	}
	if resp.Error().Error() != "missing jwt" {
		t.Fatalf("unexpected error: %s", resp.Error())
	}

	// test bad role name
	data = map[string]interface{}{
		"role": "plugin-test-bad",
		"jwt":  jwtData,
	}
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if resp == nil || !resp.IsError() {
		t.Fatal("expected error")
	}
	if resp.Error().Error() != `invalid role name "plugin-test-bad"` {
		t.Fatalf("unexpected error: %s", resp.Error())
	}

	// test bad jwt service account
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtBadServiceAccount,
	}
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(logical.ErrPermissionDenied, err) {
		t.Fatalf("unexpected error: %s", err)
	}

	// test bad jwt key
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtWithBadSigningKey,
	}
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err == nil {
		t.Fatalf("Expected error")
	} else if !errors.Is(logical.ErrPermissionDenied, err) {
		t.Fatalf("unexpected error: %s", err)
	}

	// test successful login
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test successful login for globbed name
	config = defaultTestBackendConfig()
	config.saName = testGlobbedName
	b, storage = setupBackend(t, config)

	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test successful login for globbed namespace
	config = defaultTestBackendConfig()
	config.saNamespace = testGlobbedNamespace
	b, storage = setupBackend(t, config)

	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}
}

func TestLoginWithServiceAccountAnnotations(t *testing.T) {
	config := defaultTestBackendConfig()
	config.customMetadataFromAnnotations = true
	b, storage := setupBackend(t, config)

	data := map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err := b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	if role := resp.Auth.Metadata["service_role"]; role != "authz" {
		t.Fatalf("expected service_role in Auth.Metadata, got: %s", role)
	}

	if role := resp.Auth.Alias.Metadata["service_role"]; role != "authz" {
		t.Fatalf("expected service_role in Auth.Alias.Metadata, got: %s", role)
	}

	// test that we can't overwrite service_account_name and other properties with annotations

	b.(*kubeAuthBackend).serviceAccountReaderFactory = mockServiceAccountReaderFactory(map[string]string{
		"service_account_name":        "overwritten",
		"service_account_uid":         "overwritten",
		"service_account_namespace":   "overwritten",
		"service_account_secret_name": "overwritten",
	})
	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	if val := resp.Auth.Metadata["service_account_name"]; val != "vault-auth" {
		t.Fatalf("unexpected service_account_name: %s", val)
	}
	if val := resp.Auth.Alias.Metadata["service_account_name"]; val != "vault-auth" {
		t.Fatalf("unexpected service_account_name: %s", val)
	}
	if val := resp.Auth.Metadata["service_account_namespace"]; val != "default" {
		t.Fatalf("unexpected service_account_namespace: %s", val)
	}
	if val := resp.Auth.Alias.Metadata["service_account_namespace"]; val != "default" {
		t.Fatalf("unexpected service_account_namespace: %s", val)
	}
	if val := resp.Auth.Metadata["service_account_uid"]; val != "d77f89bc-9055-11e7-a068-0800276d99bf" {
		t.Fatalf("unexpected service_account_uid: %s", val)
	}
	if val := resp.Auth.Alias.Metadata["service_account_uid"]; val != "d77f89bc-9055-11e7-a068-0800276d99bf" {
		t.Fatalf("unexpected service_account_uid: %s", val)
	}
	if val := resp.Auth.Metadata["service_account_secret_name"]; val != "vault-auth-token-t5pcn" {
		t.Fatalf("unexpected service_account_secret_name: %s", val)
	}
	if val := resp.Auth.Alias.Metadata["service_account_secret_name"]; val != "vault-auth-token-t5pcn" {
		t.Fatalf("unexpected service_account_secret_name: %s", val)
	}
}

func TestAliasLookAhead(t *testing.T) {
	testCases := map[string]struct {
		role              string
		jwt               string
		config            *testBackendConfig
		expectedAliasName string
		wantErr           error
		wantErrCode       int
	}{
		"default": {
			role:              "plugin-test",
			jwt:               jwtData,
			config:            defaultTestBackendConfig(),
			expectedAliasName: testUID,
		},
		"no_role": {
			jwt:     jwtData,
			config:  defaultTestBackendConfig(),
			wantErr: errors.New("missing role"),
		},
		"no_jwt": {
			role:    "plugin-test",
			config:  defaultTestBackendConfig(),
			wantErr: errors.New("missing jwt"),
		},
		"invalid_jwt": {
			role:        "plugin-test",
			config:      defaultTestBackendConfig(),
			jwt:         jwtBadServiceAccount,
			wantErr:     errors.New("service account name not authorized"),
			wantErrCode: http.StatusForbidden,
		},
		"wrong_namespace": {
			role: "plugin-test",
			jwt:  jwtData,
			config: func() *testBackendConfig {
				config := defaultTestBackendConfig()
				config.saNamespace = "wrong-namespace"
				return config
			}(),
			wantErr:     errors.New("namespace not authorized"),
			wantErrCode: http.StatusForbidden,
		},
		"serviceaccount_uid": {
			role: "plugin-test",
			jwt:  jwtData,
			config: &testBackendConfig{
				pems:            testDefaultPEMs,
				saName:          testName,
				saNamespace:     testNamespace,
				aliasNameSource: aliasNameSourceSAUid,
			},
			expectedAliasName: testUID,
		},
		"serviceaccount_name": {
			role: "plugin-test",
			jwt:  jwtData,
			config: &testBackendConfig{
				pems:            testDefaultPEMs,
				saName:          testName,
				saNamespace:     testNamespace,
				aliasNameSource: aliasNameSourceSAName,
			},
			expectedAliasName: fmt.Sprintf("%s/%s", testNamespace, testName),
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			b, storage := setupBackend(t, tc.config)

			req := &logical.Request{
				Operation: logical.AliasLookaheadOperation,
				Path:      "login",
				Storage:   storage,
				Data: map[string]interface{}{
					"jwt":  tc.jwt,
					"role": tc.role,
				},
				Connection: &logical.Connection{
					RemoteAddr: "127.0.0.1",
				},
			}

			resp, err := b.HandleRequest(context.Background(), req)
			if tc.wantErr != nil {
				var actual error
				if err != nil {
					actual = err
				} else if resp != nil && resp.IsError() {
					actual = resp.Error()
				} else {
					t.Fatalf("expected error")
				}

				if tc.wantErr.Error() != actual.Error() {
					t.Fatalf("expected err %q, actual %q", tc.wantErr, actual)
				}
				if tc.wantErrCode != 0 {
					requireErrorCode(t, err, tc.wantErrCode)
				}
			} else {
				if err != nil || (resp != nil && resp.IsError()) {
					t.Fatalf("err:%s resp:%#v\n", err, resp)
				}

				if resp.Auth.Alias.Name != tc.expectedAliasName {
					t.Fatalf("expected Alias.Name %s, actual %s", tc.expectedAliasName, resp.Auth.Alias.Name)
				}
			}
		})
	}
}

func TestLoginIssValidation(t *testing.T) {
	config := defaultTestBackendConfig()
	b, storage := setupBackend(t, config)

	// test iss validation enabled with default "kubernetes/serviceaccount" issuer
	data := map[string]interface{}{
		"kubernetes_host":        "host",
		"kubernetes_ca_cert":     testCACert,
		"disable_iss_validation": false,
	}

	req := &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "config",
		Storage:   storage,
		Data:      data,
	}

	resp, err := b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test successful login with default issuer
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	// test iss validation enabled with explicitly defined issuer
	data = map[string]interface{}{
		"kubernetes_host":        "host",
		"kubernetes_ca_cert":     testCACert,
		"disable_iss_validation": false,
		"issuer":                 "kubernetes/serviceaccount",
		"pem_keys":               testDefaultPEMs,
	}

	req = &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "config",
		Storage:   storage,
		Data:      data,
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test successful login with explicitly defined issuer
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test iss validation enabled with custom issuer
	data = map[string]interface{}{
		"kubernetes_host":        "host",
		"kubernetes_ca_cert":     testCACert,
		"disable_iss_validation": false,
		"issuer":                 "custom-issuer",
		"pem_keys":               testDefaultPEMs,
	}

	req = &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "config",
		Storage:   storage,
		Data:      data,
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test login fail with enabled iss validation and custom issuer
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != `invalid token issuer` {
		t.Fatalf("unexpected error: %s", err)
	}

	// test iss validation disabled with custom issuer
	data = map[string]interface{}{
		"kubernetes_host":        "host",
		"kubernetes_ca_cert":     testCACert,
		"disable_iss_validation": true,
		"issuer":                 "custom-issuer",
		"pem_keys":               testDefaultPEMs,
	}

	req = &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "config",
		Storage:   storage,
		Data:      data,
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	// test login success with disabled iss validation and custom issuer
	data = map[string]interface{}{
		"role": "plugin-test",
		"jwt":  jwtData,
	}

	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err = b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}
}

var jwtData = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJrdWJlcm5ldGVzL3NlcnZpY2VhY2NvdW50Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9uYW1lc3BhY2UiOiJkZWZhdWx0Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9zZWNyZXQubmFtZSI6InZhdWx0LWF1dGgtdG9rZW4tdDVwY24iLCJrdWJlcm5ldGVzLmlvL3NlcnZpY2VhY2NvdW50L3NlcnZpY2UtYWNjb3VudC5uYW1lIjoidmF1bHQtYXV0aCIsImt1YmVybmV0ZXMuaW8vc2VydmljZWFjY291bnQvc2VydmljZS1hY2NvdW50LnVpZCI6ImQ3N2Y4OWJjLTkwNTUtMTFlNy1hMDY4LTA4MDAyNzZkOTliZiIsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDpkZWZhdWx0OnZhdWx0LWF1dGgifQ.HKUcqgrvan5ZC_mnpaMEx4RW3KrhfyH_u8G_IA2vUfkLK8tH3T7fJuJaPr7W6K_BqCrbeM5y3owszOzb4NR0Lvw6GBt2cFcen2x1Ua4Wokr0bJjTT7xQOIOw7UvUDyVS17wAurlfUnmWMwMMMOebpqj5K1t6GnyqghH1wPdHYRGX-q5a6C323dBCgM5t6JY_zTTaBgM6EkFq0poBaifmSMiJRPrdUN_-IgyK8fgQRiFYYkgS6DMIU4k4nUOb_sUFf5xb8vMs3SMteKiuWFAIt4iszXTj5IyBUNqe0cXA3zSY3QiNCV6bJ2CWW0Qf9WDtniT79VAqcR4GYaTC_gxjNA"

var jwtBadServiceAccount = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJrdWJlcm5ldGVzL3NlcnZpY2VhY2NvdW50Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9uYW1lc3BhY2UiOiJkZWZhdWx0Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9zZWNyZXQubmFtZSI6InZhdWx0LWludmFsaWQtdG9rZW4tZ3ZxcHQiLCJrdWJlcm5ldGVzLmlvL3NlcnZpY2VhY2NvdW50L3NlcnZpY2UtYWNjb3VudC5uYW1lIjoidmF1bHQtaW52YWxpZCIsImt1YmVybmV0ZXMuaW8vc2VydmljZWFjY291bnQvc2VydmljZS1hY2NvdW50LnVpZCI6IjA0NGZkNGYxLTk3NGQtMTFlNy05YTE1LTA4MDAyNzZkOTliZiIsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDpkZWZhdWx0OnZhdWx0LWludmFsaWQifQ.BcoOdu5BrIchp66Zl8-dY7HcGHJrVXrUh4SNTlIHR6vDaNH29B7JuI_-B1pvW9GpzQnc-XjZyua_wfSssqe-KYJcq--Qh0yQfbbLE5rvEipBCHH341IqGaTHaBVip8zXqYE-bt-7J6vAH8Azvw46iatDC73tKxh46xDuxK0gKjdprW4cOklDx6ZSxEHpu63ftLYgAgk9c0MUJxKWhu9Jk0aye5pTj_iyBbBy8llZNGaw2gxvhPzFVUEHZUlTRiSIbmPmNqep48RiJoWrq6FM1lijvrtT5y-E7aFk6TpW2BH3VDHy8k10sMIxuRAYrGB3tpUKNyVDI3tJOi_xY7iJvw"

var jwtWithBadSigningKey = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJrdWJlcm5ldGVzL3NlcnZpY2VhY2NvdW50Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9uYW1lc3BhY2UiOiJkZWZhdWx0Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9zZWNyZXQubmFtZSI6InZhdWx0LWludmFsaWQtdG9rZW4tZ3ZxcHQiLCJrdWJlcm5ldGVzLmlvL3NlcnZpY2VhY2NvdW50L3NlcnZpY2UtYWNjb3VudC5uYW1lIjoidmF1bHQtYXV0aCIsImt1YmVybmV0ZXMuaW8vc2VydmljZWFjY291bnQvc2VydmljZS1hY2NvdW50LnVpZCI6IjA0NGZkNGYxLTk3NGQtMTFlNy05YTE1LTA4MDAyNzZkOTliZiIsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDpkZWZhdWx0OnZhdWx0LWF1dGgifQ.hv4O-T9XPtV3Smy55TrA2qCjRJJEQqeifqzbV1kyb8hr7o7kSqhBRy0fSWHi8rkrnBXjibB0yTDDHR1UvkHLWD2Ddi9tKeXZahaKLxGh5GJI8TSxZizX3ilZB9A5LBpW_VberSxcazhGA1u3VEPaL_nPsxWcdF9kxZR3hwSlyEA"

var jwtECDSASigned = "eyJhbGciOiJFUzM4NCIsInR5cCI6IkpXVCIsImtpZCI6ImlUcVhYSTB6YkFuSkNLRGFvYmZoa00xZi02ck1TcFRmeVpNUnBfMnRLSTgifQ.eyJrdWJlcm5ldGVzLmlvL3NlcnZpY2VhY2NvdW50L25hbWVzcGFjZSI6ImRlZmF1bHQiLCJrdWJlcm5ldGVzLmlvL3NlcnZpY2VhY2NvdW50L3NlcnZpY2UtYWNjb3VudC5uYW1lIjoidmF1bHQtYXV0aCIsImt1YmVybmV0ZXMuaW8vc2VydmljZWFjY291bnQvc2VydmljZS1hY2NvdW50LnVpZCI6ImQ3N2Y4OWJjLTkwNTUtMTFlNy1hMDY4LTA4MDAyNzZkOTliZiIsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDpkZWZhdWx0OnZhdWx0LWF1dGgiLCJpc3MiOiJrdWJlcm5ldGVzL3NlcnZpY2VhY2NvdW50In0.JYxQVgAJQhEIa1lIZ1s9SQ4IrW3FUsl7IfykYBflTgHz0CExAe5BcJ90g1eErVi1RZB1mh2pl9SjIrfFgDeRwqOYwZ4tqCr5dhcZAX5F7yt_RBuuVOvX-EGAklMo0usp"

var ecdsaKey = `-----BEGIN PUBLIC KEY-----
MHYwEAYHKoZIzj0CAQYFK4EEACIDYgAEC1uWSXj2czCDwMTLWV5BFmwxdM6PX9p+
Pk9Yf9rIf374m5XP1U8q79dBhLSIuaojsvOT39UUcPJROSD1FqYLued0rXiooIii
1D3jaW6pmGVJFhodzC31cy5sfOYotrzF
-----END PUBLIC KEY-----`

func TestLoginProjectedToken(t *testing.T) {
	config := defaultTestBackendConfig()
	config.pems = append(testDefaultPEMs, testMinikubePubKey)
	b, storage := setupBackend(t, config)

	// update backend to accept "default" bound account name
	data := map[string]interface{}{
		"bound_service_account_names":      fmt.Sprintf("%s,default", testName),
		"bound_service_account_namespaces": testNamespace,
		"policies":                         "test",
		"period":                           "3s",
		"ttl":                              "1s",
		"num_uses":                         12,
		"max_ttl":                          "5s",
	}

	req := &logical.Request{
		Operation: logical.CreateOperation,
		Path:      "role/plugin-test",
		Storage:   storage,
		Data:      data,
	}

	resp, err := b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	roleNameError := fmt.Errorf("invalid role name %q", "plugin-test-x")

	testCases := map[string]struct {
		role        string
		jwt         string
		tokenReview tokenReviewFactory
		e           error
	}{
		"normal": {
			role:        "plugin-test",
			jwt:         jwtData,
			tokenReview: testMockTokenReviewFactory,
		},
		"fail": {
			role:        "plugin-test-x",
			jwt:         jwtData,
			tokenReview: testMockTokenReviewFactory,
			e:           roleNameError,
		},
		"projected-token": {
			role:        "plugin-test",
			jwt:         jwtProjectedData,
			tokenReview: testProjectedMockFactory,
		},
		"projected-token-expired": {
			role:        "plugin-test",
			jwt:         jwtProjectedDataExpired,
			tokenReview: testProjectedMockFactory,
			e:           errors.New("Token is expired"),
		},
		"projected-token-invalid-role": {
			role:        "plugin-test-x",
			jwt:         jwtProjectedData,
			tokenReview: testProjectedMockFactory,
			e:           roleNameError,
		},
	}

	for k, tc := range testCases {
		t.Run(k, func(t *testing.T) {
			data := map[string]interface{}{
				"role": tc.role,
				"jwt":  tc.jwt,
			}

			req := &logical.Request{
				Operation: logical.UpdateOperation,
				Path:      "login",
				Storage:   storage,
				Data:      data,
				Connection: &logical.Connection{
					RemoteAddr: "127.0.0.1",
				},
			}

			b.(*kubeAuthBackend).reviewFactory = tc.tokenReview
			b.(*kubeAuthBackend).serviceAccountReaderFactory = testMockServiceAccountReaderFactory

			resp, err := b.HandleRequest(context.Background(), req)
			if err != nil && tc.e == nil {
				t.Fatalf("unexpected err: (%s) resp:%#v\n", err, resp)
			}
			if err == nil && !resp.IsError() && tc.e != nil {
				t.Fatalf("expected error but found none: (%s) resp: %#v\n", tc.e, resp)
			}
			if resp != nil && resp.IsError() {
				if tc.e == nil {
					t.Fatalf("unexpected err: (%s)\n", resp.Error())
				}
				if tc.e.Error() != resp.Error().Error() {
					t.Fatalf("error mismatch in response, expected (%s) got (%s)", tc.e, resp.Error())
				}
			}
			if resp == nil && err != nil {
				if tc.e == nil {
					t.Fatalf("unexpected err: (%s)", err)
				}
				if tc.e.Error() != err.Error() {
					t.Fatalf("error mismatch, expected (%s) got (%s)", tc.e, err)
				}
			}
		})
	}
}

func TestAliasLookAheadProjectedToken(t *testing.T) {
	config := defaultTestBackendConfig()
	config.pems = append(testDefaultPEMs, testMinikubePubKey)
	config.saName = "default"
	b, storage := setupBackend(t, config)

	data := map[string]interface{}{
		"jwt":  jwtProjectedData,
		"role": "plugin-test",
	}

	req := &logical.Request{
		Operation: logical.AliasLookaheadOperation,
		Path:      "login",
		Storage:   storage,
		Data:      data,
		Connection: &logical.Connection{
			RemoteAddr: "127.0.0.1",
		},
	}

	resp, err := b.HandleRequest(context.Background(), req)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("err:%s resp:%#v\n", err, resp)
	}

	if resp.Auth.Alias.Name != testProjectedUID {
		t.Fatalf("Unexpected UID: %s", resp.Auth.Alias.Name)
	}
}

type mockServiceAccountReader struct {
	annotations map[string]string
}

func mockServiceAccountReaderFactory(annotations map[string]string) serviceAccountReaderFactory {
	return func(config *kubeConfig) serviceAccountReader {
		return &mockServiceAccountReader{
			annotations: annotations,
		}
	}
}

func (s *mockServiceAccountReader) ReadAnnotations(ctx context.Context, name, namespace string) (map[string]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return s.annotations, nil
}

// jwtProjectedData is a Projected Service Account jwt with expiration set to
// 05 Nov 2030 04:19:57 (UTC)
//
// {
// 	"aud": [
// 	  "kubernetes.default.svc"
// 	],
// 	"exp": 1920082797,
// 	"iat": 1604082797,
// 	"iss": "kubernetes/serviceaccount",
// 	"kubernetes.io": {
// 	  "namespace": "default",
// 	  "pod": {
// 		"name": "vault",
// 		"uid": "086c2f61-dea2-47bb-b5ca-63e63c5c9885"
// 	  },
// 	  "serviceaccount": {
// 		"name": "default",
// 		"uid": "77c81ad7-1bea-4d94-9ca5-f5d7f3632331"
// 	  }
// 	},
// 	"nbf": 1604082797,
// 	"sub": "system:serviceaccount:default:default"
// }
var jwtProjectedData = "eyJhbGciOiJSUzI1NiIsImtpZCI6InBKY3hrSjRxME8xdE90MFozN1ZCNi14Nk13OHhGWlN4TTlyb1B0TVFxMEEifQ.eyJhdWQiOlsia3ViZXJuZXRlcy5kZWZhdWx0LnN2YyJdLCJleHAiOjE5MjAwODI3OTcsImlhdCI6MTYwNDA4Mjc5NywiaXNzIjoia3ViZXJuZXRlcy9zZXJ2aWNlYWNjb3VudCIsImt1YmVybmV0ZXMuaW8iOnsibmFtZXNwYWNlIjoiZGVmYXVsdCIsInBvZCI6eyJuYW1lIjoidmF1bHQiLCJ1aWQiOiIwODZjMmY2MS1kZWEyLTQ3YmItYjVjYS02M2U2M2M1Yzk4ODUifSwic2VydmljZWFjY291bnQiOnsibmFtZSI6ImRlZmF1bHQiLCJ1aWQiOiI3N2M4MWFkNy0xYmVhLTRkOTQtOWNhNS1mNWQ3ZjM2MzIzMzEifX0sIm5iZiI6MTYwNDA4Mjc5Nywic3ViIjoic3lzdGVtOnNlcnZpY2VhY2NvdW50OmRlZmF1bHQ6ZGVmYXVsdCJ9.fh9yPq8zPQR4Gms6sNpn82yppV5ONWaAVzEYnFSrOK_mM69wn51bCtdG3ARJjbBoZv6wK7bNfwSKlD3nar1QTCpyz5UKW_f_m9J7IqVdLnNIjEXhuzTv2WlxFV4VeXSYX9Q6ndUsWO-m1iKdPCkIm8sHKKv9BYVtFyhEgwSDsisX2YmseHMO8j1lpROlgrv4JvUfJ7m7tn2vV4B0WiM3djwVg2Uqv830mzZ-w0VKEuqBtUzw3zisNWa96N6DcokVebD4ZzUU2-YQPWE9ccjy0NW0frCCwFO1KiVMW9E7KTQ3qMq-B8-ZTrdV58ba-EgEnbOLsmLgp4Z_e_bmvJx4hg"

// jwtProjectedDataExpired is a Projected Service Account jwt with expiration
// set to 30 Oct 2020 18:51:26 (UTC)
//
// {
// 	"aud": [
// 	  "kubernetes.default.svc"
// 	],
// 	"exp": 1604083886,
// 	"iat": 1604083286,
// 	"iss": "kubernetes/serviceaccount",
// 	"kubernetes.io": {
// 	  "namespace": "default",
// 	  "pod": {
// 		"name": "vault",
// 		"uid": "34be4d5f-66d3-4a29-beea-ce23e51f9fb8"
// 	  },
// 	  "serviceaccount": {
// 		"name": "default",
// 		"uid": "77c81ad7-1bea-4d94-9ca5-f5d7f3632331"
// 	  }
// 	},
// 	"nbf": 1604083286,
// 	"sub": "system:serviceaccount:default:default"
// }
var jwtProjectedDataExpired = "eyJhbGciOiJSUzI1NiIsImtpZCI6InBKY3hrSjRxME8xdE90MFozN1ZCNi14Nk13OHhGWlN4TTlyb1B0TVFxMEEifQ.eyJhdWQiOlsia3ViZXJuZXRlcy5kZWZhdWx0LnN2YyJdLCJleHAiOjE2MDQwODM4ODYsImlhdCI6MTYwNDA4MzI4NiwiaXNzIjoia3ViZXJuZXRlcy9zZXJ2aWNlYWNjb3VudCIsImt1YmVybmV0ZXMuaW8iOnsibmFtZXNwYWNlIjoiZGVmYXVsdCIsInBvZCI6eyJuYW1lIjoidmF1bHQiLCJ1aWQiOiIzNGJlNGQ1Zi02NmQzLTRhMjktYmVlYS1jZTIzZTUxZjlmYjgifSwic2VydmljZWFjY291bnQiOnsibmFtZSI6ImRlZmF1bHQiLCJ1aWQiOiI3N2M4MWFkNy0xYmVhLTRkOTQtOWNhNS1mNWQ3ZjM2MzIzMzEifX0sIm5iZiI6MTYwNDA4MzI4Niwic3ViIjoic3lzdGVtOnNlcnZpY2VhY2NvdW50OmRlZmF1bHQ6ZGVmYXVsdCJ9.dpsCBhOC-7yy47JgoSN1rCafLVR_wV9drXfRPqZotj_KszG-Oyq8zO3HmZTRM7aWqwR7X-Zna04DdnIktLuLaRvLfOMkRDJsdfzsxMlRNqaxVkJq3fRYTbJwcsM9xNiquJ16lfZmQV2VE64kYFTiN_3-kkGY05z_CvzqZcEfnKhdUTuvNXIP893rdk-72kKFa1HuWz0c6vgOOoxMf4hsoNhzgVAp5P39ZpQvZLNMhwaUcbhq55WxuaGsBcm7SNLfkT-hNG06RQXhSwo_qTXo9gZzPhG7bm4nNDh_wg7b4ORQVBe00kqiFhfyH7bBdwZliKKi3xxw43wpbC2cS8nyDA"

// testMinikubePubKey is the public key of the minikube instance used to
// generate the projected token signature for jwtProjectedData and
// jwtProjectedDataExpired above.
//
// To setup a minikube instance to replicate or re-generate keys if needed, use
// this invocation:
//
// minikube start --kubernetes-version v1.18.10  \
//   --feature-gates="TokenRequest=true,TokenRequestProjection=true" \
//   --extra-config=apiserver.service-account-signing-key-file=/var/lib/minikube/certs/sa.key \
//   --extra-config=apiserver.service-account-issuer="kubernetes/serviceaccount" \
//   --extra-config=apiserver.service-account-api-audiences="kubernetes.default.svc" \
//   --extra-config=apiserver.service-account-key-file=/var/lib/minikube/certs/sa.pub
//
// When Minikube is up, use `minikube ssh` to connect and extract the contents
// of sa.pub for use here.
//
var testMinikubePubKey = `
-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAygmU/WKtGT77GhHYbEmR
DXufJVdJ3iSuooYcscFcwAUvQMpzt5Gd0kfI03dLx7o6r7z4BTeSaJ14ABPTYfAy
+U47Cf1zhlHw2pcWveRfq3lVEzlaqzD9u8ENkqBSB6guyIxM8RadiufJPHGkWPrw
fOH7VaKwuW/T//oMmZwrFwD6DF99O02hUwwvM1B7b+E1+zvH5BdMHtEzB/32ibkX
WKDrXOZIZAMPHZtt2MojxdGpPxiBSVODn6hw8n4hGBWuH7UABU+2h2kZI0ctxWaX
UIX4hSHyjlKYDGEezrUP1mm7AX5pN1qrjtxasTSPPX8nZY/3HtM77n4PfYEwCrew
rwIDAQAB
-----END PUBLIC KEY-----`

func Test_kubeAuthBackend_getAliasName(t *testing.T) {
	expectedErr := fmt.Errorf("service account namespace and name must be set")
	issuerDefault := "kubernetes/serviceaccount"
	issuerProjected := "https://kubernetes.default.svc.cluster.local"

	tests := []struct {
		name        string
		role        *roleStorageEntry
		signRequest *jwtSignTestRequest
		want        string
		wantErr     bool
	}{
		{
			name: "default",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceDefault,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerDefault,
				ns:        "default",
				sa:        "sa",
				uid:       testUID,
				projected: false,
			},
			want:    testUID,
			wantErr: false,
		},
		{
			name: "default-sa-uid",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceSAUid,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerDefault,
				ns:        "default",
				sa:        "sa",
				uid:       testUID,
				projected: false,
			},
			want:    testUID,
			wantErr: false,
		},
		{
			name: "default-sa-name",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceSAName,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerDefault,
				ns:        "default",
				sa:        "sa",
				projected: false,
			},
			want:    fmt.Sprintf("%s/%s", "default", "sa"),
			wantErr: false,
		},
		{
			name: "invalid-default-empty-ns",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceSAName,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerProjected,
				ns:        "",
				sa:        "sa2",
				projected: false,
			},
			want:    "",
			wantErr: true,
		},
		{
			name: "invalid-default-empty-sa",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceSAName,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerProjected,
				ns:        "default",
				sa:        "",
				projected: false,
			},
			want:    "",
			wantErr: true,
		},
		{
			name: "projected",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceDefault,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerProjected,
				ns:        "default",
				sa:        "sa",
				uid:       testProjectedUID,
				projected: true,
			},
			want:    testProjectedUID,
			wantErr: false,
		},
		{
			name: "projected-sa-uid",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceSAUid,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerProjected,
				ns:        "default",
				sa:        "sa",
				uid:       testProjectedUID,
				projected: true,
			},
			want:    testProjectedUID,
			wantErr: false,
		},
		{
			name: "projected-sa-name",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceSAName,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerProjected,
				ns:        "ns1",
				sa:        "sa",
				projected: true,
			},
			want:    fmt.Sprintf("%s/%s", "ns1", "sa"),
			wantErr: false,
		},
		{
			name: "invalid-projected-empty-ns",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceSAName,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerProjected,
				ns:        "",
				sa:        "sa2",
				projected: true,
			},
			want:    "",
			wantErr: true,
		},
		{
			name: "invalid-projected-empty-sa",
			role: &roleStorageEntry{
				AliasNameSource: aliasNameSourceSAName,
			},
			signRequest: &jwtSignTestRequest{
				issuer:    issuerProjected,
				ns:        "default",
				sa:        "",
				projected: true,
			},
			want:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &kubeAuthBackend{}

			s, err := signTestJWTRequest(tt.signRequest)
			if err != nil {
				t.Fatal(err)
			}

			claims := jwt.MapClaims{}
			_, _, err = jwt.NewParser().ParseUnverified(s, &claims)
			if err != nil {
				t.Fatal(err)
			}

			sa := &serviceAccount{}
			if err := mapstructure.Decode(claims, sa); err != nil {
				t.Fatal(err)
			}

			got, err := b.getAliasName(tt.role, sa)

			if tt.wantErr {
				if err == nil {
					t.Errorf("getAliasName() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
				if !reflect.DeepEqual(expectedErr, err) {
					t.Errorf("getAliasName() expected error = %v, actual %v", expectedErr, err)
				}
			}

			if got != tt.want {
				t.Errorf("getAliasName() got = %v, want %v", got, tt.want)
			}
		})
	}
}

type jwtSignTestRequest struct {
	ns        string
	sa        string
	uid       string
	projected bool
	issuer    string
	expired   bool
}

func (r *jwtSignTestRequest) getUID() string {
	var uid string
	if r.uid == "" {
		uid, _ = uuid.GenerateUUID()
		r.uid = uid
	}

	return r.uid
}

func signTestJWTRequest(req *jwtSignTestRequest) (string, error) {
	var claims jwt.Claims
	if req.projected {
		claims = projectedJWTTestClaims(req)
	} else {
		claims = defaultJWTTestClaims(req)
	}

	return signTestJWT(claims)
}

func jwtStandardTestClaims(req *jwtSignTestRequest) jwt.StandardClaims {
	now := time.Now()
	var horizon int64 = 86400
	if req.expired {
		horizon = horizon * -1
	}
	return jwt.StandardClaims{
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Unix() + horizon,
		Issuer:    req.issuer,
	}
}

func projectedJWTTestClaims(req *jwtSignTestRequest) jwt.Claims {
	type testToken struct {
		Namespace      string         `json:"namespace"`
		Pod            *v1.ObjectMeta `json:"pod"`
		ServiceAccount *v1.ObjectMeta `json:"serviceaccount"`
	}

	type Claims struct {
		Audiences []string   `json:"aud"`
		Token     *testToken `json:"kubernetes.io"`
		jwt.StandardClaims
	}

	uid := types.UID(req.getUID())
	return &Claims{
		Audiences: []string{"baz"},
		Token: &testToken{
			Namespace: req.ns,
			Pod: &v1.ObjectMeta{
				Name: "pod",
				UID:  uid,
			},
			ServiceAccount: &v1.ObjectMeta{
				Name: req.sa,
				UID:  uid,
			},
		},
		StandardClaims: jwtStandardTestClaims(req),
	}
}

func defaultJWTTestClaims(req *jwtSignTestRequest) jwt.Claims {
	type Claims struct {
		Namespace          string `json:"kubernetes.io/serviceaccount/namespace"`
		SecretName         string `json:"kubernetes.io/serviceaccount/secret.name"`
		ServiceAccountName string `json:"kubernetes.io/serviceaccount/service-account.name"`
		UID                string `json:"kubernetes.io/serviceaccount/service-account.uid"`
		Sub                string `json:"sub"`
		jwt.StandardClaims
	}

	return &Claims{
		Namespace:          req.ns,
		ServiceAccountName: req.sa,
		UID:                req.getUID(),
		StandardClaims:     jwtStandardTestClaims(req),
	}
}

func signTestJWT(claims jwt.Claims) (string, error) {
	pkey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	return token.SignedString(pkey)
}

func requireErrorCode(t *testing.T, err error, expectedCode int) {
	t.Helper()

	codedErr, ok := err.(logical.HTTPCodedError)
	switch {
	case ok && codedErr.Code() == expectedCode:
		// Happy case
	case !ok:
		t.Fatal("err was not logical.HTTPCodedError")
	default:
		t.Fatalf("wrong error code, expected %d, got %d", expectedCode, codedErr.Code())
	}
}
