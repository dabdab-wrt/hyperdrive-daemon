package tests

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/nodeset-org/beacon-mock/db"
	"github.com/nodeset-org/beacon-mock/manager"
	"github.com/nodeset-org/hyperdrive-daemon/common"
	"github.com/nodeset-org/hyperdrive-daemon/internal/docker"
	"github.com/nodeset-org/hyperdrive-daemon/shared/config"
	"github.com/rocket-pool/node-manager-core/beacon/client"
	"github.com/rocket-pool/node-manager-core/node/services"
)

const (
	// The environment variable for the locally running Hardhat instance
	HardhatEnvVar string = "HARDHAT_URL"
)

// TestManager provides bootstrapping and a test service provider, useful for testing
type TestManager struct {
	// The service provider for the test environment
	ServiceProvider *common.ServiceProvider

	// Logger for logging output messages during tests
	Logger *slog.Logger

	// The Hyperdrive user directory
	testingConfigDir string

	// RPC client for running Hardhat's admin functions
	hardhatRpcClient *rpc.Client

	// Beacon mock manager for running BN admin functions
	beaconMockManager *manager.BeaconMockManager

	// Snapshot ID from the baseline - the initial state of Hardhat prior to running any of the tests in this package
	baselineSnapshotID string
}

// Creates a new TestManager instance
func NewTestManager() (*TestManager, error) {
	// Make sure the Hardhat URL
	hardhatUrl, exists := os.LookupEnv(HardhatEnvVar)
	if !exists {
		return nil, fmt.Errorf("%s env var not set", HardhatEnvVar)
	}

	// Make a new logger
	logger := slog.Default()

	// Create a temp folder
	var err error
	testingConfigDir, err := os.MkdirTemp("", "hd-tests-*")
	if err != nil {
		return nil, fmt.Errorf("error creating temp config dir: %v", err)
	}
	logger.Info("Created temp config dir", "dir", testingConfigDir)

	// Create the default Beacon config
	beaconCfg := db.NewDefaultConfig()

	// Make a new Hyperdrive config
	cfg := config.NewHyperdriveConfig(testingConfigDir)
	cfg.Network.Value = config.Network_LocalTest

	// Make test resources
	resources := GetTestResources(beaconCfg)

	// Make the RPC client for the Hardhat instance (used for admin functions)
	hardhatRpcClient, err := rpc.Dial(hardhatUrl)
	if err != nil {
		cleanup(testingConfigDir)
		return nil, fmt.Errorf("error creating RPC client binding: %w", err)
	}

	// Make the Execution client manager
	clientTimeout := time.Duration(10) * time.Second
	primaryEc, err := ethclient.Dial(hardhatUrl)
	if err != nil {
		cleanup(testingConfigDir)
		return nil, fmt.Errorf("error creating primary eth client with URL [%s]: %v", hardhatUrl, err)
	}
	ecManager := services.NewExecutionClientManager(primaryEc, uint(beaconCfg.ChainID), clientTimeout)

	// Make the Beacon client manager
	beaconMockManager := manager.NewBeaconMockManager(logger, beaconCfg)
	primaryBn := client.NewStandardClient(beaconMockManager)
	bnManager := services.NewBeaconClientManager(primaryBn, uint(beaconCfg.ChainID), clientTimeout)

	// Make a Docker client mock
	docker := docker.NewDockerClientMock()

	// Make a new service provider
	serviceProvider, err := common.NewServiceProviderFromCustomServices(
		cfg,
		resources,
		ecManager,
		bnManager,
		docker,
	)
	if err != nil {
		cleanup(testingConfigDir)
		return nil, fmt.Errorf("error creating service provider: %v", err)
	}

	m := &TestManager{
		ServiceProvider:   serviceProvider,
		Logger:            logger,
		testingConfigDir:  testingConfigDir,
		hardhatRpcClient:  hardhatRpcClient,
		beaconMockManager: beaconMockManager,
	}

	// Create the baseline snapshot
	baselineSnapshotID, err := m.takeSnapshot()
	if err != nil {
		return nil, fmt.Errorf("error creating baseline snapshot: %w", err)
	}
	m.baselineSnapshotID = baselineSnapshotID

	// Return
	return m, nil
}

// Prints an error message to stderr and exits the program with an error code
func (m *TestManager) Fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	m.Cleanup()
	os.Exit(1)
}

// Cleans up the test environment, including the temporary folder to house any generated files
func (m *TestManager) Cleanup() {
	err := m.RevertToBaseline()
	if err != nil {
		m.Logger.Error(err.Error())
	}
	if m.testingConfigDir == "" {
		return
	}
	cleanup(m.testingConfigDir)
	m.testingConfigDir = ""
}

// Reverts the EC and BN to the baseline snapshot
func (m *TestManager) RevertToBaseline() error {
	err := m.revertToSnapshot(m.baselineSnapshotID)
	if err != nil {
		return fmt.Errorf("error reverting to baseline snapshot: %w", err)
	}

	// Regenerate the baseline snapshot since Hardhat can't revert to it multiple times
	baselineSnapshotID, err := m.takeSnapshot()
	if err != nil {
		return fmt.Errorf("error creating baseline snapshot: %w", err)
	}
	m.baselineSnapshotID = baselineSnapshotID
	return nil
}

// Takes a snapshot of the EC and BN states
func (m *TestManager) CreateCustomSnapshot() (string, error) {
	return m.takeSnapshot()
}

// Revert the EC and BN to a snapshot state
func (m *TestManager) RevertToCustomSnapshot(snapshotID string) error {
	return m.revertToSnapshot(snapshotID)
}

// Takes a snapshot of the EC and BN states
func (m *TestManager) takeSnapshot() (string, error) {
	// Snapshot the EC
	var snapshotName string
	err := m.hardhatRpcClient.Call(&snapshotName, "evm_snapshot")
	if err != nil {
		return "", fmt.Errorf("error creating snapshot: %w", err)
	}

	// Snapshot the BN
	m.beaconMockManager.TakeSnapshot(snapshotName)
	return snapshotName, nil
}

// Revert the EC and BN to a snapshot state
func (m *TestManager) revertToSnapshot(snapshotID string) error {
	// Revert the EC
	err := m.hardhatRpcClient.Call(nil, "evm_revert", snapshotID)
	if err != nil {
		return fmt.Errorf("error reverting Hardhat to snapshot %s: %w", snapshotID, err)
	}

	// Revert the BN
	err = m.beaconMockManager.RevertToSnapshot(snapshotID)
	if err != nil {
		return fmt.Errorf("error reverting the BN to snapshot %s: %w", snapshotID, err)
	}
	return nil
}

// Delete the test config dir
func cleanup(testingConfigDir string) {
	err := os.RemoveAll(testingConfigDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "error removing temp config dir [%s]: %v", testingConfigDir, err)
	}
}
