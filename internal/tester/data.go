package tester

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"path"
	"time"

	"github.com/coinbase/rosetta-cli/configuration"
	"github.com/coinbase/rosetta-cli/internal/logger"
	"github.com/coinbase/rosetta-cli/internal/processor"
	"github.com/coinbase/rosetta-cli/internal/statefulsyncer"
	"github.com/coinbase/rosetta-cli/internal/storage"
	"github.com/coinbase/rosetta-cli/internal/utils"

	"github.com/coinbase/rosetta-sdk-go/fetcher"
	"github.com/coinbase/rosetta-sdk-go/reconciler"
	"github.com/coinbase/rosetta-sdk-go/types"
	"github.com/fatih/color"
	"golang.org/x/sync/errgroup"
)

const (
	// InactiveFailureLookbackWindow is the size of each window to check
	// for missing ops. If a block with missing ops is not found in this
	// window, another window is created with the preceding
	// InactiveFailureLookbackWindow blocks (this process continues
	// until the client halts the search or the block is found).
	InactiveFailureLookbackWindow = 250

	// PeriodicLoggingFrequency is the frequency that stats are printed
	// to the terminal.
	//
	// TODO: make configurable
	PeriodicLoggingFrequency = 10 * time.Second
)

type DataTester struct {
	network           *types.NetworkIdentifier
	config            *configuration.Configuration
	syncer            *statefulsyncer.StatefulSyncer
	reconciler        *reconciler.Reconciler
	logger            *logger.Logger
	counterStorage    *storage.CounterStorage
	reconcilerHandler *processor.ReconcilerHandler
	reconcile         bool
	fetcher           *fetcher.Fetcher
	signalReceived    *bool
	genesisBlock      *types.BlockIdentifier
}

// loadAccounts is a utility function to parse the []*reconciler.AccountCurrency
// in a file.
func loadAccounts(filePath string) ([]*reconciler.AccountCurrency, error) {
	if len(filePath) == 0 {
		return []*reconciler.AccountCurrency{}, nil
	}

	accounts := []*reconciler.AccountCurrency{}
	if err := utils.LoadAndParse(filePath, &accounts); err != nil {
		return nil, fmt.Errorf("%w: unable to open account file", err)
	}

	log.Printf(
		"Found %d accounts at %s: %s\n",
		len(accounts),
		filePath,
		types.PrettyPrintStruct(accounts),
	)

	return accounts, nil
}

func InitializeData(
	ctx context.Context,
	config *configuration.Configuration,
	network *types.NetworkIdentifier,
	fetcher *fetcher.Fetcher,
	cancel context.CancelFunc,
	genesisBlock *types.BlockIdentifier,
	reconcile bool,
	interestingAccount *reconciler.AccountCurrency,
	signalReceived *bool,
) *DataTester {
	// Create a unique path for invocation to avoid collision when parsing
	// multiple networks.
	dataPath := path.Join(config.Data.DataDirectory, "data", types.Hash(network))
	if err := utils.EnsurePathExists(dataPath); err != nil {
		log.Fatalf("%s: cannot populate path", err.Error())
	}

	localStore, err := storage.NewBadgerStorage(ctx, dataPath)
	if err != nil {
		log.Fatalf("%s: unable to initialize database", err.Error())
	}
	defer localStore.Close(ctx)

	exemptAccounts, err := loadAccounts(config.Data.ExemptAccounts)
	if err != nil {
		log.Fatalf("%s: unable to load exempt accounts", err.Error())
	}

	interestingAccounts, err := loadAccounts(config.Data.InterestingAccounts)
	if err != nil {
		log.Fatalf("%s: unable to load interesting accounts", err.Error())
	}

	counterStorage := storage.NewCounterStorage(localStore)
	blockStorage := storage.NewBlockStorage(localStore)
	balanceStorage := storage.NewBalanceStorage(localStore)

	logger := logger.NewLogger(
		counterStorage,
		dataPath,
		config.Data.LogBlocks,
		config.Data.LogTransactions,
		config.Data.LogBalanceChanges,
		config.Data.LogReconciliations,
	)

	reconcilerHelper := processor.NewReconcilerHelper(
		blockStorage,
		balanceStorage,
	)

	reconcilerHandler := processor.NewReconcilerHandler(
		logger,
		!config.Data.IgnoreReconciliationError,
	)

	// Get all previously seen accounts
	seenAccounts, err := balanceStorage.GetAllAccountCurrency(ctx)
	if err != nil {
		log.Fatalf("%s: unable to get previously seen accounts", err.Error())
	}

	r := reconciler.New(
		network,
		reconcilerHelper,
		reconcilerHandler,
		fetcher,
		reconciler.WithActiveConcurrency(int(config.Data.ActiveReconciliationConcurrency)),
		reconciler.WithInactiveConcurrency(int(config.Data.InactiveReconciliationConcurrency)),
		reconciler.WithLookupBalanceByBlock(!config.Data.HistoricalBalanceDisabled),
		reconciler.WithInterestingAccounts(interestingAccounts),
		reconciler.WithSeenAccounts(seenAccounts),
		reconciler.WithDebugLogging(config.Data.LogReconciliations),
		reconciler.WithInactiveFrequency(int64(config.Data.InactiveReconciliationFrequency)),
	)

	balanceStorageHelper := processor.NewBalanceStorageHelper(
		network,
		fetcher,
		!config.Data.HistoricalBalanceDisabled,
		exemptAccounts,
	)

	balanceStorageHandler := processor.NewBalanceStorageHandler(
		logger,
		r,
		reconcile,
		interestingAccount,
	)

	balanceStorage.Initialize(balanceStorageHelper, balanceStorageHandler)

	// Bootstrap balances if provided
	if len(config.Data.BootstrapBalances) > 0 {
		_, err := blockStorage.GetHeadBlockIdentifier(ctx)
		if err == storage.ErrHeadBlockNotFound {
			err = balanceStorage.BootstrapBalances(
				ctx,
				config.Data.BootstrapBalances,
				genesisBlock,
			)
			if err != nil {
				log.Fatalf("%s: unable to bootstrap balances", err.Error())
			}
		} else {
			log.Println("Skipping balance bootstrapping because already started syncing")
			return nil
		}
	}

	syncer := statefulsyncer.New(
		ctx,
		network,
		fetcher,
		blockStorage,
		logger,
		cancel,
		[]storage.BlockWorker{balanceStorage},
	)

	return &DataTester{
		network:           network,
		config:            config,
		syncer:            syncer,
		reconciler:        r,
		logger:            logger,
		counterStorage:    counterStorage,
		reconcilerHandler: reconcilerHandler,
		reconcile:         reconcile,
		fetcher:           fetcher,
		signalReceived:    signalReceived,
		genesisBlock:      genesisBlock,
	}
}

func (t *DataTester) StartSyncing(
	ctx context.Context,
	startIndex int64,
	endIndex int64,
) error {
	return t.syncer.Sync(ctx, startIndex, endIndex)
}

func (t *DataTester) StartReconciler(
	ctx context.Context,
) error {
	if !t.reconcile {
		return nil
	}

	return t.reconciler.Reconcile(ctx)
}

func (t *DataTester) StartPeriodicLogger(
	ctx context.Context,
) error {
	for ctx.Err() == nil {
		_ = t.logger.LogCounterStorage(ctx)
		time.Sleep(PeriodicLoggingFrequency)
	}

	// Print stats one last time before exiting
	_ = t.logger.LogCounterStorage(ctx)

	return nil
}

func (t *DataTester) HandleErr(ctx context.Context, err error) {
	if *t.signalReceived {
		color.Red("Check halted")
		os.Exit(1)
		return
	}

	if err == nil || err == context.Canceled { // err == context.Canceled when --end
		activeReconciliations, activeErr := t.counterStorage.Get(
			ctx,
			storage.ActiveReconciliationCounter,
		)
		inactiveReconciliations, inactiveErr := t.counterStorage.Get(
			ctx,
			storage.InactiveReconciliationCounter,
		)

		if activeErr != nil || inactiveErr != nil ||
			new(big.Int).Add(activeReconciliations, inactiveReconciliations).Sign() != 0 {
			color.Green("Check succeeded")
		} else { // warn caller when check succeeded but no reconciliations performed (as issues may still exist)
			color.Yellow("Check succeeded, however, no reconciliations were performed!")
		}
		os.Exit(0)
	}

	color.Red("Check failed: %s", err.Error())
	if t.reconcilerHandler.InactiveFailure == nil {
		os.Exit(1)
	}

	if t.config.Data.HistoricalBalanceDisabled {
		color.Red(
			"Can't find the block missing operations automatically, please enable --lookup-balance-by-block",
		)
		os.Exit(1)
	}
}

// FindMissingOps logs the types.BlockIdentifier of a block
// that is missing balance-changing operations for a
// *reconciler.AccountCurrency.
func (t *DataTester) FindMissingOps(ctx context.Context, sigListeners []context.CancelFunc) {
	color.Red("Searching for block with missing operations...hold tight")
	badBlock, err := t.recursiveOpSearch(
		ctx,
		&sigListeners,
		t.reconcilerHandler.InactiveFailure,
		t.reconcilerHandler.InactiveFailureBlock.Index-InactiveFailureLookbackWindow,
		t.reconcilerHandler.InactiveFailureBlock.Index,
	)
	if err != nil {
		color.Red("%s: could not find block with missing ops", err.Error())
		os.Exit(1)
	}

	color.Red(
		"Missing ops for %s in block %d:%s",
		types.AccountString(t.reconcilerHandler.InactiveFailure.Account),
		badBlock.Index,
		badBlock.Hash,
	)
	os.Exit(1)
}

func (t *DataTester) recursiveOpSearch(
	ctx context.Context,
	sigListeners *[]context.CancelFunc,
	accountCurrency *reconciler.AccountCurrency,
	startIndex int64,
	endIndex int64,
) (*types.BlockIdentifier, error) {
	// To cancel all execution, need to call multiple cancel functions.
	ctx, cancel := context.WithCancel(ctx)
	*sigListeners = append(*sigListeners, cancel)

	// Always use a temporary directory to find missing ops
	tmpDir, err := utils.CreateTempDir()
	if err != nil {
		return nil, fmt.Errorf("%w: unable to create temporary directory", err)
	}
	defer utils.RemoveTempDir(tmpDir)

	localStore, err := storage.NewBadgerStorage(ctx, tmpDir)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to initialize database", err)
	}

	counterStorage := storage.NewCounterStorage(localStore)
	blockStorage := storage.NewBlockStorage(localStore)
	balanceStorage := storage.NewBalanceStorage(localStore)

	logger := logger.NewLogger(
		counterStorage,
		tmpDir,
		false,
		false,
		false,
		false,
	)

	balanceStorageHelper := processor.NewBalanceStorageHelper(
		t.network,
		t.fetcher,
		!t.config.Data.HistoricalBalanceDisabled,
		nil,
	)

	reconcilerHelper := processor.NewReconcilerHelper(
		blockStorage,
		balanceStorage,
	)

	reconcilerHandler := processor.NewReconcilerHandler(
		logger,
		true, // halt on reconciliation error
	)

	r := reconciler.New(
		t.network,
		reconcilerHelper,
		reconcilerHandler,
		t.fetcher,

		// When using concurrency > 1, we could start looking up balance changes
		// on multiple blocks at once. This can cause us to return the wrong block
		// that is missing operations.
		reconciler.WithActiveConcurrency(1),

		// Do not do any inactive lookups when looking for the block with missing
		// operations.
		reconciler.WithInactiveConcurrency(0),
		reconciler.WithLookupBalanceByBlock(!t.config.Data.HistoricalBalanceDisabled),
		reconciler.WithInterestingAccounts([]*reconciler.AccountCurrency{accountCurrency}),
	)

	balanceStorageHandler := processor.NewBalanceStorageHandler(
		logger,
		r,
		true,
		accountCurrency,
	)

	balanceStorage.Initialize(balanceStorageHelper, balanceStorageHandler)

	syncer := statefulsyncer.New(
		ctx,
		t.network,
		t.fetcher,
		blockStorage,
		logger,
		cancel,
		[]storage.BlockWorker{balanceStorage},
	)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return r.Reconcile(ctx)
	})

	g.Go(func() error {
		return syncer.Sync(
			ctx,
			startIndex,
			endIndex,
		)
	})

	err = g.Wait()

	// Close database before starting another search, otherwise we will
	// have n databases open when we find the offending block.
	if storageErr := localStore.Close(ctx); storageErr != nil {
		return nil, fmt.Errorf("%w: unable to close database", storageErr)
	}

	if *t.signalReceived {
		return nil, errors.New("Search for block with missing ops halted")
	}

	if err == nil || err == context.Canceled {
		newStart := startIndex - InactiveFailureLookbackWindow
		if newStart < t.genesisBlock.Index {
			newStart = t.genesisBlock.Index
		}

		newEnd := endIndex - InactiveFailureLookbackWindow
		if newEnd <= newStart {
			return nil, fmt.Errorf(
				"Next window to check has start index %d <= end index %d",
				newStart,
				newEnd,
			)
		}

		color.Red(
			"Unable to find missing ops in block range %d-%d, now searching %d-%d",
			startIndex, endIndex,
			newStart,
			newEnd,
		)

		return t.recursiveOpSearch(
			// We need to use new context for each invocation because the syncer
			// cancels the provided context when it reaches the end of a syncing
			// window.
			context.Background(),
			sigListeners,
			accountCurrency,
			startIndex-InactiveFailureLookbackWindow,
			endIndex-InactiveFailureLookbackWindow,
		)
	}

	if reconcilerHandler.ActiveFailureBlock == nil {
		return nil, errors.New("unable to find missing ops")
	}

	return reconcilerHandler.ActiveFailureBlock, nil
}
