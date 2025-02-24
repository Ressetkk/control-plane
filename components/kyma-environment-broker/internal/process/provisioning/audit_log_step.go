package provisioning

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal/auditlog"

	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal"
	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal/process"
	"github.com/kyma-project/control-plane/components/kyma-environment-broker/internal/storage"
	"github.com/kyma-project/control-plane/components/provisioner/pkg/gqlschema"
	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
)

type AuditLogOverrides struct {
	operationManager *process.ProvisionOperationManager
	fs               afero.Fs
	auditLogConfig   auditlog.Config
}

func (alo *AuditLogOverrides) Name() string {
	return "Audit_Log_Overrides"
}

func NewAuditLogOverridesStep(fileSystem afero.Fs, os storage.Operations, cfg auditlog.Config) *AuditLogOverrides {
	return &AuditLogOverrides{
		process.NewProvisionOperationManager(os),
		fileSystem,
		cfg,
	}
}

func (alo *AuditLogOverrides) Run(operation internal.ProvisioningOperation, logger logrus.FieldLogger) (internal.ProvisioningOperation, time.Duration, error) {
	if alo.auditLogConfig.Disabled {
		logger.Infof("Skipping appending auditlog overrides")
		operation.InputCreator.AppendOverrides("logging", []*gqlschema.ConfigEntryInput{})
		return operation, 0, nil
	}
	luaScript, err := alo.readFile("/auditlog-script/script")
	if err != nil {
		logger.Errorf("Unable to read audit config script: %v", err)
		return operation, 0, err
	}

	replaceSubAccountID := strings.Replace(string(luaScript), "sub_account_id", operation.ProvisioningParameters.ErsContext.SubAccountID, -1)
	replaceTenantID := strings.Replace(replaceSubAccountID, "tenant_id", alo.auditLogConfig.Tenant, -1)

	u, err := url.Parse(alo.auditLogConfig.URL)
	if err != nil {
		logger.Errorf("Unable to parse the URL: %v", err.Error())
		return operation, 0, err
	}
	if u.Path == "" {
		logger.Errorf("There is no Path passed in the URL")
		return operation, 0, errors.New("there is no Path passed in the URL")
	}
	auditLogHost, auditLogPort, err := net.SplitHostPort(u.Host)
	if err != nil {
		logger.Errorf("Unable to split URL: %v", err.Error())
		return operation, 0, err
	}
	if auditLogPort == "" {
		auditLogPort = "443"
		logger.Infof("There is no Port passed in the URL. Setting default to 443")
	}
	fluentbitPlugin := "http"
	if alo.auditLogConfig.EnableSeqHttp {
		fluentbitPlugin = "sequentialhttp"
	}

	operation.InputCreator.AppendOverrides("logging", []*gqlschema.ConfigEntryInput{
		{Key: "fluent-bit.config.script", Value: replaceTenantID},
		{Key: "fluent-bit.config.extra", Value: fmt.Sprintf(`
[INPUT]
    Name              tail
    Tag               dex.*
    Path              /var/log/containers/*_dex-*.log
    DB                /var/log/flb_kube_dex.db
    parser            docker
    Mem_Buf_Limit     5MB
    Skip_Long_Lines   On
    Refresh_Interval  10
[FILTER]
    Name    lua
    Match   dex.*
    script  script.lua
    call    reformat
[FILTER]
    Name    grep
    Match   dex.*
    Regex   time .*
[FILTER]
    Name    grep
    Match   dex.*
    Regex   data .*\"xsuaa
[OUTPUT]
    Name             %s
    Match            dex.*
    Retry_Limit      False
    Host             %s
    Port             %s
    URI              %ssecurity-events
    Header           Content-Type application/json
    HTTP_User        ${AUDITLOG_USER}
    HTTP_Passwd      ${AUDITLOG_PASSWD}
    Format           json_stream
    tls              on
`, fluentbitPlugin, auditLogHost, auditLogPort, u.Path)},
		{Key: "fluent-bit.config.secrets.AUDITLOG_USER", Value: fmt.Sprintf(`%s`, alo.auditLogConfig.User)},
		{Key: "fluent-bit.config.secrets.AUDITLOG_PASSWD", Value: fmt.Sprintf(`%s`, alo.auditLogConfig.Password)},
		{Key: "fluent-bit.externalServiceEntry.resolution", Value: "DNS"},
		{Key: "fluent-bit.externalServiceEntry.hosts", Value: fmt.Sprintf(`- %s`, auditLogHost)},
		{Key: "fluent-bit.externalServiceEntry.ports", Value: fmt.Sprintf(`- number: %s
  name: https
  protocol: TLS`, auditLogPort)},
	})
	return operation, 0, nil
}

func (alo *AuditLogOverrides) readFile(fileName string) ([]byte, error) {
	return afero.ReadFile(alo.fs, fileName)
}
