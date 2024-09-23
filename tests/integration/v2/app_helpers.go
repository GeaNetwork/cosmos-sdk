// this file is a port of testutil/sims/app_helpers.go from v1 to v2 architecture
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cosmossdk.io/core/comet"
	corecontext "cosmossdk.io/core/context"
	"cosmossdk.io/core/event"
	"cosmossdk.io/core/server"
	corestore "cosmossdk.io/core/store"
	"cosmossdk.io/core/transaction"
	"cosmossdk.io/depinject"
	sdkmath "cosmossdk.io/math"
	"cosmossdk.io/runtime/v2"
	"cosmossdk.io/runtime/v2/services"
	"cosmossdk.io/server/v2/stf"
	"cosmossdk.io/server/v2/stf/branch"
	banktypes "cosmossdk.io/x/bank/types"
	consensustypes "cosmossdk.io/x/consensus/types"
	stakingtypes "cosmossdk.io/x/staking/types"
	cmtproto "github.com/cometbft/cometbft/api/cometbft/types/v1"
	cmtjson "github.com/cometbft/cometbft/libs/json"
	cmttypes "github.com/cometbft/cometbft/types"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/cosmos/cosmos-sdk/std"
	"github.com/cosmos/cosmos-sdk/testutil/mock"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
)

const DefaultGenTxGas = 10000000

type stateMachineTx = transaction.Tx

// DefaultConsensusParams defines the default CometBFT consensus params used in
// SimApp testing.
var DefaultConsensusParams = &cmtproto.ConsensusParams{
	Version: &cmtproto.VersionParams{
		App: 1,
	},
	Block: &cmtproto.BlockParams{
		MaxBytes: 200000,
		MaxGas:   100_000_000,
	},
	Evidence: &cmtproto.EvidenceParams{
		MaxAgeNumBlocks: 302400,
		MaxAgeDuration:  504 * time.Hour, // 3 weeks is the max duration
		MaxBytes:        10000,
	},
	Validator: &cmtproto.ValidatorParams{
		PubKeyTypes: []string{
			cmttypes.ABCIPubKeyTypeEd25519,
			cmttypes.ABCIPubKeyTypeSecp256k1,
		},
	},
}

// CreateRandomValidatorSet creates a validator set with one random validator
func CreateRandomValidatorSet() (*cmttypes.ValidatorSet, error) {
	privVal := mock.NewPV()
	pubKey, err := privVal.GetPubKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get pub key: %w", err)
	}

	// create validator set with single validator
	validator := cmttypes.NewValidator(pubKey, 1)

	return cmttypes.NewValidatorSet([]*cmttypes.Validator{validator}), nil
}

type GenesisAccount struct {
	authtypes.GenesisAccount
	Coins sdk.Coins
}

// StartupConfig defines the startup configuration new a test application.
//
// ValidatorSet defines a custom validator set to be validating the app.
// BaseAppOption defines the additional operations that must be run on baseapp before app start.
// AtGenesis defines if the app started should already have produced block or not.
type StartupConfig struct {
	ValidatorSet    func() (*cmttypes.ValidatorSet, error)
	AppOption       runtime.AppBuilderOption[stateMachineTx]
	AtGenesis       bool
	GenesisAccounts []GenesisAccount
	HomeDir         string
}

func DefaultStartUpConfig() StartupConfig {
	priv := secp256k1.GenPrivKey()
	ba := authtypes.NewBaseAccount(
		priv.PubKey().Address().Bytes(),
		priv.PubKey(),
		0,
		0,
	)
	ga := GenesisAccount{
		ba,
		sdk.NewCoins(
			sdk.NewCoin(sdk.DefaultBondDenom, sdkmath.NewInt(100000000000000)),
		),
	}
	return StartupConfig{
		ValidatorSet:    CreateRandomValidatorSet,
		AtGenesis:       false,
		GenesisAccounts: []GenesisAccount{ga},
	}
}

// Setup initializes a new runtime.App and can inject values into extraOutputs.
// It uses SetupWithConfiguration under the hood.
func Setup(
	appConfig depinject.Config,
	extraOutputs ...interface{},
) (*App, error) {
	return SetupWithConfiguration(
		appConfig,
		DefaultStartUpConfig(),
		extraOutputs...)
}

// SetupAtGenesis initializes a new runtime.App at genesis and can inject values into extraOutputs.
// It uses SetupWithConfiguration under the hood.
func SetupAtGenesis(
	appConfig depinject.Config,
	extraOutputs ...interface{},
) (*App, error) {
	cfg := DefaultStartUpConfig()
	cfg.AtGenesis = true
	return SetupWithConfiguration(appConfig, cfg, extraOutputs...)
}

var _ server.DynamicConfig = &dynamicConfigImpl{}

type dynamicConfigImpl struct {
	homeDir string
}

func (d *dynamicConfigImpl) Get(key string) any {
	return d.GetString(key)
}

func (d *dynamicConfigImpl) GetString(key string) string {
	switch key {
	case runtime.FlagHome:
		return d.homeDir
	case "store.app-db-backend":
		return "goleveldb"
	case "server.minimum-gas-prices":
		return "0stake"
	default:
		panic(fmt.Sprintf("unknown key: %s", key))
	}
}

func (d *dynamicConfigImpl) UnmarshalSub(string, any) (bool, error) {
	return false, nil
}

var _ comet.Service = &cometServiceImpl{}

type cometServiceImpl struct{}

func (c cometServiceImpl) CometInfo(context.Context) comet.Info {
	return comet.Info{}
}

// SetupWithConfiguration initializes a new runtime.App. A Nop logger is set in runtime.App.
// appConfig defines the application configuration (f.e. app_config.go).
// extraOutputs defines the extra outputs to be assigned by the dependency injector (depinject).
func SetupWithConfiguration(
	appConfig depinject.Config,
	startupConfig StartupConfig,
	extraOutputs ...interface{},
) (*App, error) {
	// create the app with depinject
	var (
		app             *runtime.App[stateMachineTx]
		appBuilder      *runtime.AppBuilder[stateMachineTx]
		storeBuilder    *runtime.StoreBuilder
		txConfigOptions tx.ConfigOptions
		cometService    comet.Service                   = &cometServiceImpl{}
		kvFactory       corestore.KVStoreServiceFactory = func(actor []byte) corestore.KVStoreService {
			return services.NewGenesisKVService(actor, &storeService{actor, stf.NewKVStoreService(actor)})
		}
		cdc codec.Codec
		err error
	)

	if err := depinject.Inject(
		depinject.Configs(
			appConfig,
			codec.DefaultProviders,
			depinject.Supply(
				services.NewGenesisHeaderService(stf.HeaderService{}),
				&dynamicConfigImpl{startupConfig.HomeDir},
				cometService,
				kvFactory,
				&eventService{},
			),
			depinject.Invoke(
				std.RegisterInterfaces,
			),
		),
		append(extraOutputs, &appBuilder, &cdc, &txConfigOptions, &storeBuilder)...); err != nil {
		return nil, fmt.Errorf("failed to inject dependencies: %w", err)
	}

	app, err = appBuilder.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build app: %w", err)
	}
	if err := app.LoadLatest(); err != nil {
		return nil, fmt.Errorf("failed to load app: %w", err)
	}

	// create validator set
	valSet, err := startupConfig.ValidatorSet()
	if err != nil {
		return nil, errors.New("failed to create validator set")
	}

	var (
		balances    []banktypes.Balance
		genAccounts []authtypes.GenesisAccount
	)
	for _, ga := range startupConfig.GenesisAccounts {
		genAccounts = append(genAccounts, ga.GenesisAccount)
		balances = append(
			balances,
			banktypes.Balance{
				Address: ga.GenesisAccount.GetAddress().String(),
				Coins:   ga.Coins,
			},
		)
	}

	genesisJSON, err := genesisStateWithValSet(
		cdc,
		app.DefaultGenesis(),
		valSet,
		genAccounts,
		balances...)
	if err != nil {
		return nil, fmt.Errorf("failed to create genesis state: %w", err)
	}

	// init chain must be called to stop deliverState from being nil
	genesisJSONBytes, err := cmtjson.MarshalIndent(genesisJSON, "", " ")
	if err != nil {
		return nil, fmt.Errorf(
			"failed to marshal default genesis state: %w",
			err,
		)
	}

	ctx := context.WithValue(
		context.Background(),
		corecontext.CometParamsInitInfoKey,
		&consensustypes.MsgUpdateParams{
			Authority: "consensus",
			Block:     DefaultConsensusParams.Block,
			Evidence:  DefaultConsensusParams.Evidence,
			Validator: DefaultConsensusParams.Validator,
			Abci:      DefaultConsensusParams.Abci,
			Synchrony: DefaultConsensusParams.Synchrony,
			Feature:   DefaultConsensusParams.Feature,
		},
	)

	store := storeBuilder.Get()
	if store == nil {
		return nil, fmt.Errorf("failed to build store: %w", err)
	}
	err = store.SetInitialVersion(1)
	if err != nil {
		return nil, fmt.Errorf("failed to set initial version: %w", err)
	}
	integrationApp := &App{App: app, Store: store}

	emptyHash := sha256.Sum256(nil)
	_, genesisState, err := app.InitGenesis(
		ctx,
		&server.BlockRequest[stateMachineTx]{
			Height:    1,
			Time:      time.Now(),
			Hash:      emptyHash[:],
			ChainId:   "test-chain",
			AppHash:   emptyHash[:],
			IsGenesis: true,
		},
		genesisJSONBytes,
		&genericTxDecoder{txConfigOptions},
	)
	if err != nil {
		return nil, fmt.Errorf("failed init genesiss: %w", err)
	}

	genesisChanges, err := genesisState.GetStateChanges()
	if err != nil {
		return nil, fmt.Errorf("failed to get genesis state changes: %w", err)
	}
	cs := &corestore.Changeset{Changes: genesisChanges}
	for _, change := range genesisChanges {
		if !bytes.Equal(change.Actor, []byte("acc")) {
			continue
		}
		for _, kv := range change.StateChanges {
			fmt.Printf("actor: %s, key: %x, value: %x\n", change.Actor, kv.Key, kv.Value)
		}
	}
	_, err = store.Commit(cs)
	if err != nil {
		return nil, fmt.Errorf("failed to commit initial version: %w", err)
	}

	return integrationApp, nil
}

// genesisStateWithValSet returns a new genesis state with the validator set
func genesisStateWithValSet(
	codec codec.Codec,
	genesisState map[string]json.RawMessage,
	valSet *cmttypes.ValidatorSet,
	genAccs []authtypes.GenesisAccount,
	balances ...banktypes.Balance,
) (map[string]json.RawMessage, error) {
	// set genesis accounts
	authGenesis := authtypes.NewGenesisState(authtypes.DefaultParams(), genAccs)
	genesisState[authtypes.ModuleName] = codec.MustMarshalJSON(authGenesis)

	validators := make([]stakingtypes.Validator, 0, len(valSet.Validators))
	delegations := make([]stakingtypes.Delegation, 0, len(valSet.Validators))

	bondAmt := sdk.DefaultPowerReduction

	for _, val := range valSet.Validators {
		pk, err := cryptocodec.FromCmtPubKeyInterface(val.PubKey)
		if err != nil {
			return nil, fmt.Errorf("failed to convert pubkey: %w", err)
		}

		pkAny, err := codectypes.NewAnyWithValue(pk)
		if err != nil {
			return nil, fmt.Errorf("failed to create new any: %w", err)
		}

		validator := stakingtypes.Validator{
			OperatorAddress: sdk.ValAddress(val.Address).String(),
			ConsensusPubkey: pkAny,
			Jailed:          false,
			Status:          stakingtypes.Bonded,
			Tokens:          bondAmt,
			DelegatorShares: sdkmath.LegacyOneDec(),
			Description:     stakingtypes.Description{},
			UnbondingHeight: int64(0),
			UnbondingTime:   time.Unix(0, 0).UTC(),
			Commission: stakingtypes.NewCommission(
				sdkmath.LegacyZeroDec(),
				sdkmath.LegacyZeroDec(),
				sdkmath.LegacyZeroDec(),
			),
			MinSelfDelegation: sdkmath.ZeroInt(),
		}
		validators = append(validators, validator)
		delegations = append(
			delegations,
			stakingtypes.NewDelegation(
				genAccs[0].GetAddress().String(),
				sdk.ValAddress(val.Address).String(),
				sdkmath.LegacyOneDec(),
			),
		)

	}

	// set validators and delegations
	stakingGenesis := stakingtypes.NewGenesisState(
		stakingtypes.DefaultParams(),
		validators,
		delegations,
	)
	genesisState[stakingtypes.ModuleName] = codec.MustMarshalJSON(
		stakingGenesis,
	)

	totalSupply := sdk.NewCoins()
	for _, b := range balances {
		// add genesis acc tokens to total supply
		totalSupply = totalSupply.Add(b.Coins...)
	}

	for range delegations {
		// add delegated tokens to total supply
		totalSupply = totalSupply.Add(
			sdk.NewCoin(sdk.DefaultBondDenom, bondAmt),
		)
	}

	// add bonded amount to bonded pool module account
	balances = append(balances, banktypes.Balance{
		Address: authtypes.NewModuleAddress(stakingtypes.BondedPoolName).
			String(),
		Coins: sdk.Coins{sdk.NewCoin(sdk.DefaultBondDenom, bondAmt)},
	})

	// update total supply
	bankGenesis := banktypes.NewGenesisState(
		banktypes.DefaultGenesisState().Params,
		balances,
		totalSupply,
		[]banktypes.Metadata{},
		[]banktypes.SendEnabled{},
	)
	genesisState[banktypes.ModuleName] = codec.MustMarshalJSON(bankGenesis)

	return genesisState, nil
}

type genericTxDecoder struct {
	tx.ConfigOptions
}

// Decode implements transaction.Codec.
func (t *genericTxDecoder) Decode(bz []byte) (stateMachineTx, error) {
	var out stateMachineTx
	tx, err := t.ProtoDecoder(bz)
	if err != nil {
		return out, err
	}

	var ok bool
	out, ok = tx.(stateMachineTx)
	if !ok {
		return out, errors.New("unexpected Tx type")
	}

	return out, nil
}

// DecodeJSON implements transaction.Codec.
func (t *genericTxDecoder) DecodeJSON(bz []byte) (stateMachineTx, error) {
	var out stateMachineTx
	tx, err := t.JSONDecoder(bz)
	if err != nil {
		return out, err
	}

	var ok bool
	out, ok = tx.(stateMachineTx)
	if !ok {
		return out, errors.New("unexpected Tx type")
	}

	return out, nil
}

type App struct {
	*runtime.App[stateMachineTx]
	Store runtime.Store
}

type storeService struct {
	actor            []byte
	executionService corestore.KVStoreService
}

type contextKeyType struct{}

var contextKey = contextKeyType{}

type integrationContext struct {
	state corestore.WriterMap
}

func (s storeService) OpenKVStore(ctx context.Context) corestore.KVStore {
	iCtx, ok := ctx.Value(contextKey).(integrationContext)
	if !ok {
		return s.executionService.OpenKVStore(ctx)
	}

	state, err := iCtx.state.GetWriter(s.actor)
	if err != nil {
		panic(err)
	}
	return state
}

var (
	_ event.Service = &eventService{}
	_ event.Manager = &eventManager{}
)

type eventService struct{}

// EventManager implements event.Service.
func (e *eventService) EventManager(context.Context) event.Manager {
	return &eventManager{}
}

type eventManager struct{}

// Emit implements event.Manager.
func (e *eventManager) Emit(event transaction.Msg) error {
	return nil
}

// EmitKV implements event.Manager.
func (e *eventManager) EmitKV(eventType string, attrs ...event.Attribute) error {
	return nil
}

func (a *App) Run(
	ctx context.Context,
	state corestore.ReaderMap,
	fn func(ctx context.Context) error,
) (corestore.ReaderMap, error) {
	nextState := branch.DefaultNewWriterMap(state)
	iCtx := integrationContext{state: nextState}
	ctx = context.WithValue(ctx, contextKey, iCtx)
	err := fn(ctx)
	if err != nil {
		return nil, err
	}
	return nextState, nil
}
