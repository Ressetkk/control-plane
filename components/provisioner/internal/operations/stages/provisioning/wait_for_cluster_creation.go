package provisioning

import (
	"context"
	"errors"
	"fmt"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardener_types "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	"github.com/kyma-project/control-plane/components/provisioner/internal/model"
	"github.com/kyma-project/control-plane/components/provisioner/internal/operations"
	"github.com/kyma-project/control-plane/components/provisioner/internal/provisioning/persistence/dbsession"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type WaitForClusterCreationStep struct {
	gardenerClient     GardenerClient
	dbSession          dbsession.ReadWriteSession
	kubeconfigProvider KubeconfigProvider
	nextStep           model.OperationStage
	timeLimit          time.Duration
}

//go:generate mockery -name=KubeconfigProvider
type KubeconfigProvider interface {
	FetchRaw(shootName string) ([]byte, error)
}

func NewWaitForClusterCreationStep(gardenerClient GardenerClient, dbSession dbsession.ReadWriteSession, kubeconfigProvider KubeconfigProvider, nextStep model.OperationStage, timeLimit time.Duration) *WaitForClusterCreationStep {
	return &WaitForClusterCreationStep{
		gardenerClient:     gardenerClient,
		dbSession:          dbSession,
		kubeconfigProvider: kubeconfigProvider,

		nextStep:  nextStep,
		timeLimit: timeLimit,
	}
}

func (s *WaitForClusterCreationStep) Name() model.OperationStage {
	return model.WaitingForClusterCreation
}

func (s *WaitForClusterCreationStep) TimeLimit() time.Duration {
	return s.timeLimit
}

func (s *WaitForClusterCreationStep) Run(cluster model.Cluster, _ model.Operation, logger log.FieldLogger) (operations.StageResult, error) {
	shoot, err := s.gardenerClient.Get(context.Background(), cluster.ClusterConfig.Name, v1.GetOptions{})
	if err != nil {
		return operations.StageResult{}, err
	}

	lastOperation := shoot.Status.LastOperation

	if lastOperation != nil {
		if lastOperation.State == gardencorev1beta1.LastOperationStateSucceeded {
			return s.proceedToInstallation(cluster, shoot)
		}

		if lastOperation.State == gardencorev1beta1.LastOperationStateFailed {
			if gardencorev1beta1helper.HasErrorCode(shoot.Status.LastErrors, gardencorev1beta1.ErrorInfraRateLimitsExceeded) {
				return operations.StageResult{}, errors.New("error during cluster provisioning: rate limits exceeded")
			}

			logger.Warningf("Provisioning failed! Last state: %s, Description: %s", lastOperation.State, lastOperation.Description)

			err := errors.New(fmt.Sprintf("cluster provisioning failed. Last Shoot state: %s, Shoot description: %s", lastOperation.State, lastOperation.Description))

			return operations.StageResult{}, operations.NewNonRecoverableError(err)
		}
	}

	return operations.StageResult{Stage: s.Name(), Delay: 20 * time.Second}, nil
}

func (s *WaitForClusterCreationStep) proceedToInstallation(cluster model.Cluster, shoot *gardener_types.Shoot) (operations.StageResult, error) {

	if cluster.ClusterConfig.Seed == "" && shoot.Spec.SeedName != nil && *shoot.Spec.SeedName != "" {

		cluster.ClusterConfig.Seed = *shoot.Spec.SeedName

		dberr := s.dbSession.UpdateGardenerClusterConfig(cluster.ClusterConfig)

		if dberr != nil {
			return operations.StageResult{}, dberr
		}
	}

	kubeconfig, err := s.kubeconfigProvider.FetchRaw(shoot.Name)
	if err != nil {
		return operations.StageResult{}, err
	}

	dberr := s.dbSession.UpdateKubeconfig(cluster.ID, string(kubeconfig))
	if dberr != nil {
		return operations.StageResult{}, dberr
	}

	return operations.StageResult{Stage: s.nextStep, Delay: 0}, nil
}
