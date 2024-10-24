package connection_test

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Layr-Labs/eigensdk-go/chainio/clients/eth"
	"github.com/Layr-Labs/eigensdk-go/crypto/bls"
	rpccalls "github.com/Layr-Labs/eigensdk-go/metrics/collectors/rpc_calls"
	eigentypes "github.com/Layr-Labs/eigensdk-go/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	aggregator "github.com/yetanotherco/aligned_layer/aggregator/pkg"
	servicemanager "github.com/yetanotherco/aligned_layer/contracts/bindings/AlignedLayerServiceManager"
	connection "github.com/yetanotherco/aligned_layer/core"
	"github.com/yetanotherco/aligned_layer/core/chainio"
	"github.com/yetanotherco/aligned_layer/core/config"
	"github.com/yetanotherco/aligned_layer/core/utils"
)

func DummyFunction(x uint64) (uint64, error) {
	fmt.Println("Starting Anvil on Port ")
	if x == 42 {
		return 0, connection.PermanentError{Inner: fmt.Errorf("Permanent error!")}
	} else if x < 42 {
		return 0, fmt.Errorf("Transient error!")
	}
	return x, nil
}

func TestRetryWithData(t *testing.T) {
	function := func() (*uint64, error) {
		x, err := DummyFunction(43)
		return &x, err
	}
	data, err := connection.RetryWithData(function, 1000, 2, 3)
	if err != nil {
		t.Errorf("Retry error!: %s", err)
	} else {
		fmt.Printf("DATA: %d\n", data)
	}
}

func TestRetry(t *testing.T) {
	function := func() error {
		_, err := DummyFunction(43)
		return err
	}
	err := connection.Retry(function, 1000, 2, 3)
	if err != nil {
		t.Errorf("Retry error!: %s", err)
	}
}

/*
Starts an anvil instance via the cli.
Assumes that anvil is installed but checks.
*/
func SetupAnvil(port uint16) (*exec.Cmd, *eth.InstrumentedClient, error) {

	path, err := exec.LookPath("anvil")
	if err != nil {
		fmt.Printf("Could not find `anvil` executable in `%s`\n", path)
	}

	port_str := strconv.Itoa(int(port))
	http_rpc_url := fmt.Sprintf("http://localhost:%d", port)

	// Create a command
	cmd := exec.Command("anvil", "--port", port_str, "--load-state", "../contracts/scripts/anvil/state/alignedlayer-deployed-anvil-state.json", "--block-time", "3")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Run the command
	err = cmd.Start()
	if err != nil {
		fmt.Printf("Error: %s\n", err)
	}

	// Delay needed for anvil to start
	time.Sleep(1 * time.Second)

	reg := prometheus.NewRegistry()
	rpcCallsCollector := rpccalls.NewCollector("ethRpc", reg)
	ethRpcClient, err := eth.NewInstrumentedClient(http_rpc_url, rpcCallsCollector)
	if err != nil {
		log.Fatal("Error initializing eth rpc client: ", err)
	}

	return cmd, ethRpcClient, nil
}

func TestAnvilSetupKill(t *testing.T) {
	// Start Anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		log.Fatal("Error setting up Anvil: ", err)
	}

	// Get Anvil PID
	pid := cmd.Process.Pid
	p, err := os.FindProcess(pid)
	if err != nil {
		log.Fatal("Error finding Anvil Process: ", err)
	}
	err = p.Signal(syscall.Signal(0))
	assert.Nil(t, err, "Anvil Process Killed")

	// Kill Anvil
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("Error killing process: %v\n", err)
		return
	}

	// Check that PID is not currently present in running processes.
	// FindProcess always succeeds so on Unix systems we call it below.
	p, err = os.FindProcess(pid)
	if err != nil {
		log.Fatal("Error finding Anvil Process: ", err)
	}
	// Ensure process has exited
	err = p.Signal(syscall.Signal(0))

	assert.Nil(t, err, "Anvil Process Killed")
}

// |--Aggreagator Retry Tests--|

// Waits for receipt from anvil node -> Will fail to get receipt
func TestWaitForTransactionReceiptRetryable(t *testing.T) {

	// Retry call Params
	to := common.BytesToAddress([]byte{0x11})
	tx := types.NewTx(&types.AccessListTx{
		ChainID:  big.NewInt(1337),
		Nonce:    1,
		GasPrice: big.NewInt(11111),
		Gas:      1111,
		To:       &to,
		Value:    big.NewInt(111),
		Data:     []byte{0x11, 0x11, 0x11},
	})

	ctx := context.WithoutCancel(context.Background())

	hash := tx.Hash()

	// Start anvil
	cmd, client, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	// Assert Call succeeds when Anvil running
	_, err = utils.WaitForTransactionReceiptRetryable(*client, ctx, hash)
	assert.NotNil(t, err, "Error Waiting for Transaction with Anvil Running: %s\n", err)
	if err.Error() != "not found" {
		fmt.Printf("WaitForTransactionReceipt Emitted incorrect error: %s\n", err)
		return
	}

	// Kill Anvil
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)

	// Errors out but "not found"
	receipt, err := utils.WaitForTransactionReceiptRetryable(*client, ctx, hash)
	assert.Nil(t, receipt, "Receipt not empty")
	assert.NotEqual(t, err.Error(), "not found")

	// Start anvil
	cmd, client, err = SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	_, err = utils.WaitForTransactionReceiptRetryable(*client, ctx, hash)
	assert.NotNil(t, err, "Call to Anvil failed")
	if err.Error() != "not found" {
		fmt.Printf("WaitForTransactionReceipt Emitted incorrect error: %s\n", err)
	}

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}

func TestInitializeNewTaskRetryable(t *testing.T) {

	//Start Anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	//Start Aggregator
	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	agg, err := aggregator.NewAggregator(*aggregatorConfig)
	if err != nil {
		aggregatorConfig.BaseConfig.Logger.Error("Cannot create aggregator", "err", err)
		return
	}
	quorumNums := eigentypes.QuorumNums{eigentypes.QuorumNum(byte(0))}
	quorumThresholdPercentages := eigentypes.QuorumThresholdPercentages{eigentypes.QuorumThresholdPercentage(byte(57))}

	// Should succeed with err msg
	err = agg.InitializeNewTaskRetryable(0, 1, quorumNums, quorumThresholdPercentages, 1*time.Second)
	assert.Nil(t, err)
	// TODO: Find exact error to assert

	// Kill Anvil
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)

	err = agg.InitializeNewTaskRetryable(0, 1, quorumNums, quorumThresholdPercentages, 1*time.Second)
	assert.NotNil(t, err)
	fmt.Printf("Error setting Avs Subscriber: %s\n", err)

	// Start Anvil
	cmd, _, err = SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	// Should succeed
	err = agg.InitializeNewTaskRetryable(0, 1, quorumNums, quorumThresholdPercentages, 1*time.Second)
	assert.Nil(t, err)
	fmt.Printf("Error setting Avs Subscriber: %s\n", err)

	if err = cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}

// |--Server Retry Tests--|
func TestProcessNewSignatureRetryable(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	//Start Aggregator
	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	agg, err := aggregator.NewAggregator(*aggregatorConfig)
	if err != nil {
		aggregatorConfig.BaseConfig.Logger.Error("Cannot create aggregator", "err", err)
		return
	}
	zero_bytes := [32]byte{}
	zero_sig := bls.NewZeroSignature()
	eigen_bytes := eigentypes.Bytes32{}

	err = agg.ProcessNewSignatureRetryable(context.Background(), 0, zero_bytes, zero_sig, eigen_bytes)
	assert.Nil(t, err)
	// TODO: Find exact error to assert

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
	/*

		err = agg.ProcessNewSignatureRetryable(context.Background(), 0, zero_bytes, zero_sig, eigen_bytes)
		assert.NotNil(t, err)
		fmt.Printf("Error Processing New Signature: %s\n", err)

		// Start anvil
		cmd, _, err = SetupAnvil(8545)
		if err != nil {
			fmt.Printf("Error setting up Anvil: %s\n", err)
		}

		err = agg.ProcessNewSignatureRetryable(context.Background(), 0, zero_bytes, zero_sig, eigen_bytes)
		assert.Nil(t, err)
		fmt.Printf("Error Processing New Signature: %s\n", err)

		// Kill Anvil at end of test
		if err := cmd.Process.Kill(); err != nil {
			fmt.Printf("error killing process: %v\n", err)
			return
		}
	*/
}

// |--AVS-Subscriber Retry Tests--|

func TestSubscribeToNewTasksV3Retryable(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	channel := make(chan *servicemanager.ContractAlignedLayerServiceManagerNewBatchV3)
	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	baseConfig := aggregatorConfig.BaseConfig
	s, err := chainio.NewAvsServiceBindings(
		baseConfig.AlignedLayerDeploymentConfig.AlignedLayerServiceManagerAddr,
		baseConfig.AlignedLayerDeploymentConfig.AlignedLayerOperatorStateRetrieverAddr,
		baseConfig.EthWsClient, baseConfig.EthWsClientFallback, baseConfig.Logger)
	if err != nil {
		fmt.Printf("Error setting up Avs Service Bindings: %s\n", err)
	}

	fmt.Printf("Subscribing")
	_, err = chainio.SubscribeToNewTasksV3Retryable(s.ServiceManager, channel, baseConfig.Logger)
	assert.Nil(t, err)
	// TODO: Find exact error to assert

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)

	/*
		_, err = chainio.SubscribeToNewTasksV3Retryable(s.ServiceManager, channel, baseConfig.Logger)
		assert.NotNil(t, err)
		fmt.Printf("Error setting Avs Subscriber: %s\n", err)

		// Start anvil
		cmd, _, err = SetupAnvil(8545)
		if err != nil {
			fmt.Printf("Error setting up Anvil: %s\n", err)
		}

		_, err = chainio.SubscribeToNewTasksV3Retryable(s.ServiceManager, channel, baseConfig.Logger)
		assert.Nil(t, err)
		fmt.Printf("Error setting Avs Subscriber: %s\n", err)

		// Kill Anvil at end of test
		if err := cmd.Process.Kill(); err != nil {
			fmt.Printf("error killing process: %v\n", err)
			return
		}
	*/
}

func TestSubscribeToNewTasksV2(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	channel := make(chan *servicemanager.ContractAlignedLayerServiceManagerNewBatchV3)
	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	baseConfig := aggregatorConfig.BaseConfig
	s, err := chainio.NewAvsServiceBindings(
		baseConfig.AlignedLayerDeploymentConfig.AlignedLayerServiceManagerAddr,
		baseConfig.AlignedLayerDeploymentConfig.AlignedLayerOperatorStateRetrieverAddr,
		baseConfig.EthWsClient, baseConfig.EthWsClientFallback, baseConfig.Logger)
	if err != nil {
		fmt.Printf("Error setting up Avs Service Bindings: %s\n", err)
	}

	_, err = chainio.SubscribeToNewTasksV3Retryable(s.ServiceManager, channel, baseConfig.Logger)
	assert.Nil(t, err)
	// TODO: Find exact error to assert

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)

	/*
		_, err = chainio.SubscribeToNewTasksV3Retryable(s.ServiceManager, channel, baseConfig.Logger)
		assert.NotNil(t, err)
		fmt.Printf("Error setting Avs Subscriber: %s\n", err)

		// Start anvil
		cmd, _, err = SetupAnvil(8545)
		if err != nil {
			fmt.Printf("Error setting up Anvil: %s\n", err)
		}

		_, err = chainio.SubscribeToNewTasksV3Retryable(s.ServiceManager, channel, baseConfig.Logger)
		assert.Nil(t, err)
		fmt.Printf("Error setting Avs Subscriber: %s\n", err)

		// Kill Anvil at end of test
		if err := cmd.Process.Kill(); err != nil {
			fmt.Printf("error killing process: %v\n", err)
			return
		}
	*/
}

func TestBlockNumber(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	//channel := make(chan *servicemanager.ContractAlignedLayerServiceManagerNewBatchV3)
	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	sub, err := chainio.NewAvsSubscriberFromConfig(aggregatorConfig.BaseConfig)
	if err != nil {
		return
	}
	_, err = sub.BlockNumberRetryable(context.Background())
	assert.Nil(t, err, "Failed to Retrieve Block Number")

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}

func TestFilterBatchV2(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	avsSubscriber, err := chainio.NewAvsSubscriberFromConfig(aggregatorConfig.BaseConfig)
	if err != nil {
		return
	}
	_, err = avsSubscriber.FilterBatchV2Retryable(0, context.Background())
	assert.Nil(t, err, "Failed to Retrieve Block Number")
	// TODO: Find exact error to assert

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}

func TestFilterBatchV3(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	avsSubscriber, err := chainio.NewAvsSubscriberFromConfig(aggregatorConfig.BaseConfig)
	if err != nil {
		return
	}
	_, err = avsSubscriber.FilterBatchV3Retryable(0, context.Background())
	//TODO: Find error to assert
	assert.NotNil(t, err, "Succeeded in filtering logs")

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}

func TestBatchesStateSubscriber(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	avsSubscriber, err := chainio.NewAvsSubscriberFromConfig(aggregatorConfig.BaseConfig)
	if err != nil {
		return
	}

	zero_bytes := [32]byte{}
	_, err = avsSubscriber.BatchesStateRetryable(nil, zero_bytes)
	//TODO: Find exact failure error
	assert.NotNil(t, err, "BatchesState")

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}

func TestSubscribeNewHead(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	c := make(chan *types.Header)
	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	avsSubscriber, err := chainio.NewAvsSubscriberFromConfig(aggregatorConfig.BaseConfig)
	if err != nil {
		return
	}

	avsSubscriber.SubscribeNewHeadRetryable(context.Background(), c)
	assert.Nil(t, err, "Should be 0")

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}

/*
// |--AVS-Writer Retry Tests--|

func TestRespondToTaskV2(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	w, err := chainio.NewAvsWriterFromConfig(aggregatorConfig.BaseConfig, aggregatorConfig.EcdsaConfig)
	txOpts := *w.Signer.GetTxOpts()
	aggregator_address := common.HexToAddress("0x0")
	zero_bytes := [32]byte{}

	w.RespondToTaskV2Retryable(&txOpts, zero_bytes, aggregator_address)
	assert.Nil(t, err, "Should be 0")

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}
*/

func TestBatchesStateWriter(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	avsWriter, err := chainio.NewAvsWriterFromConfig(aggregatorConfig.BaseConfig, aggregatorConfig.EcdsaConfig)
	if err != nil {
		return
	}
	zero_bytes := [32]byte{}

	_, err = avsWriter.BatchesStateRetryable(zero_bytes)
	assert.Nil(t, err, "Should be 0")

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}

func TestBalanceAt(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	avsWriter, err := chainio.NewAvsWriterFromConfig(aggregatorConfig.BaseConfig, aggregatorConfig.EcdsaConfig)
	if err != nil {
		return
	}
	//TODO: Source Aggregator Address
	aggregator_address := common.HexToAddress("0x0")

	_, err = avsWriter.BalanceAtRetryable(context.Background(), aggregator_address, big.NewInt(0))
	assert.Nil(t, err, "Should be 0")

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}

func TestBatchersBalances(t *testing.T) {
	// Start anvil
	cmd, _, err := SetupAnvil(8545)
	if err != nil {
		fmt.Printf("Error setting up Anvil: %s\n", err)
	}

	aggregatorConfig := config.NewAggregatorConfig("../config-files/config-aggregator-test.yaml")
	avsWriter, err := chainio.NewAvsWriterFromConfig(aggregatorConfig.BaseConfig, aggregatorConfig.EcdsaConfig)
	if err != nil {
		return
	}
	//TODO: Source real one
	sender_address := common.HexToAddress("0x0")

	_, err = avsWriter.BatcherBalancesRetryable(sender_address)
	assert.Nil(t, err, "Should be 0")

	// Kill Anvil at end of test
	if err := cmd.Process.Kill(); err != nil {
		fmt.Printf("error killing process: %v\n", err)
		return
	}
	time.Sleep(2 * time.Second)
}
