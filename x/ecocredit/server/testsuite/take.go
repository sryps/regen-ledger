package testsuite

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/orm/model/ormdb"
	"github.com/cosmos/cosmos-sdk/orm/model/ormtable"
	"github.com/cosmos/cosmos-sdk/orm/types/ormjson"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	paramstypes "github.com/cosmos/cosmos-sdk/x/params/types"
	params "github.com/cosmos/cosmos-sdk/x/params/types/proposal"
	"github.com/golang/mock/gomock"
	marketApi "github.com/regen-network/regen-ledger/api/regen/ecocredit/marketplace/v1"
	api "github.com/regen-network/regen-ledger/api/regen/ecocredit/v1"
	"github.com/regen-network/regen-ledger/types"
	"github.com/regen-network/regen-ledger/types/math"
	"github.com/regen-network/regen-ledger/types/testutil"
	"github.com/regen-network/regen-ledger/x/ecocredit"
	"github.com/regen-network/regen-ledger/x/ecocredit/basket"
	"github.com/regen-network/regen-ledger/x/ecocredit/core"
	"github.com/regen-network/regen-ledger/x/ecocredit/marketplace"
	"github.com/regen-network/regen-ledger/x/ecocredit/mocks"
	"github.com/stretchr/testify/suite"
	dbm "github.com/tendermint/tm-db"
)

type TakeTestSuite struct {
	suite.Suite

	fixtureFactory testutil.FixtureFactory
	fixture        testutil.Fixture

	codec             *codec.ProtoCodec
	sdkCtx            sdk.Context
	ctx               context.Context
	msgClient         core.MsgClient
	marketServer      marketServer
	basketServer      basketServer
	queryClient       core.QueryClient
	paramsQueryClient params.QueryClient
	signers           []sdk.AccAddress
	basketFee         sdk.Coin

	paramSpace    paramstypes.Subspace
	bankKeeper    bankkeeper.Keeper
	accountKeeper authkeeper.AccountKeeper
	mockDist      *mocks.MockDistributionKeeper

	genesisCtx types.Context
	blockTime  time.Time
}

func NewTakeTestSuite(fixtureFactory testutil.FixtureFactory, paramSpace paramstypes.Subspace, bankKeeper bankkeeper.BaseKeeper, accountKeeper authkeeper.AccountKeeper, distKeeper *mocks.MockDistributionKeeper) *TakeTestSuite {
	return &TakeTestSuite{
		fixtureFactory: fixtureFactory,
		paramSpace:     paramSpace,
		bankKeeper:     bankKeeper,
		accountKeeper:  accountKeeper,
		mockDist:       distKeeper,
	}
}

func (s *TakeTestSuite) SetupSuite() {
	sdk.SetCoinDenomRegex(func() string {
		return `[a-zA-Z][a-zA-Z0-9/:._-]{2,127}`
	})

	s.fixture = s.fixtureFactory.Setup()

	s.codec = s.fixture.Codec()

	s.blockTime = time.Now().UTC()

	// TODO clean up once types.Context merged upstream into sdk.Context
	sdkCtx := s.fixture.Context().(types.Context).WithBlockTime(s.blockTime)
	s.sdkCtx, _ = sdkCtx.CacheContext()
	s.ctx = sdk.WrapSDKContext(s.sdkCtx)
	s.genesisCtx = types.Context{Context: sdkCtx}

	_, err := s.fixture.InitGenesis(s.sdkCtx, map[string]json.RawMessage{ecocredit.ModuleName: s.ecocreditGenesis()})
	s.Require().NoError(err)

	ecocreditParams := core.DefaultParams()
	s.basketFee = sdk.NewInt64Coin("bfee", 20)
	ecocreditParams.BasketFee = sdk.NewCoins(s.basketFee)
	s.paramSpace.SetParamSet(s.sdkCtx, &ecocreditParams)

	s.signers = s.fixture.Signers()
	s.Require().GreaterOrEqual(len(s.signers), 8)
	s.basketServer = basketServer{basket.NewQueryClient(s.fixture.QueryConn()), basket.NewMsgClient(s.fixture.TxConn())}
	s.marketServer = marketServer{marketplace.NewQueryClient(s.fixture.QueryConn()), marketplace.NewMsgClient(s.fixture.TxConn())}
	s.msgClient = core.NewMsgClient(s.fixture.TxConn())
	s.queryClient = core.NewQueryClient(s.fixture.QueryConn())
	s.paramsQueryClient = params.NewQueryClient(s.fixture.QueryConn())
}

func (s *TakeTestSuite) ecocreditGenesis() json.RawMessage {
	// setup temporary mem db
	db := dbm.NewMemDB()
	defer func() {
		if err := db.Close(); err != nil {
			panic(err)
		}
	}()
	backend := ormtable.NewBackend(ormtable.BackendOptions{
		CommitmentStore: db,
		IndexStore:      db,
	})
	modDB, err := ormdb.NewModuleDB(&ecocredit.ModuleSchema, ormdb.ModuleDBOptions{})
	s.Require().NoError(err)
	ormCtx := ormtable.WrapContextDefault(backend)
	ss, err := api.NewStateStore(modDB)
	s.Require().NoError(err)
	ms, err := marketApi.NewStateStore(modDB)
	s.Require().NoError(err)

	err = ms.AllowedDenomTable().Insert(ormCtx, &marketApi.AllowedDenom{
		BankDenom:    sdk.DefaultBondDenom,
		DisplayDenom: sdk.DefaultBondDenom,
	})
	s.Require().NoError(err)

	err = ss.CreditTypeTable().Insert(ormCtx, &api.CreditType{
		Abbreviation: "C",
		Name:         "carbon",
		Unit:         "metric ton C02",
		Precision:    6,
	})
	s.Require().NoError(err)

	// export genesis into target
	target := ormjson.NewRawMessageTarget()
	err = modDB.ExportJSON(ormCtx, target)
	s.Require().NoError(err)

	// merge the params into the json target
	coreParams := core.DefaultParams()
	err = core.MergeParamsIntoTarget(s.codec, &coreParams, target)
	s.Require().NoError(err)

	// get raw json from target
	ecoJsn, err := target.JSON()
	s.Require().NoError(err)

	// set the module genesis
	return ecoJsn
}

func (s *TakeTestSuite) TestBasketScenario() {
	require := s.Require()
	user := s.signers[0]

	// create a class and issue a batch
	userTotalCreditBalance, err := math.NewDecFromString("1000000000000000")
	require.NoError(err)
	classId, batchDenom := s.createClassAndIssueBatch(user, user, "C", userTotalCreditBalance.String(), "2020-01-01", "2022-01-01")

	// fund account to create a basket
	balanceBefore := sdk.NewInt64Coin(s.basketFee.Denom, 30000)
	s.fundAccount(user, sdk.NewCoins(balanceBefore))
	s.mockDist.EXPECT().FundCommunityPool(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(interface{}, interface{}, interface{}) error {
		err := s.bankKeeper.SendCoinsFromAccountToModule(s.sdkCtx, user, ecocredit.ModuleName, sdk.NewCoins(s.basketFee))
		return err
	})

	// create a basket
	resR, err := s.basketServer.Create(s.ctx, &basket.MsgCreate{
		Curator:           s.signers[0].String(),
		Name:              "test",
		Exponent:          12,
		DisableAutoRetire: true,
		CreditTypeAbbrev:  "C",
		AllowedClasses:    []string{classId},
		DateCriteria:      nil,
		Fee:               sdk.NewCoins(s.basketFee),
	})
	require.NoError(err)
	basketDenom := resR.BasketDenom

	creditsToDeposit := math.NewDecFromInt64(3)

	// put some credits in the basket
	_, err = s.basketServer.Put(s.ctx, &basket.MsgPut{
		Owner:       user.String(),
		BasketDenom: basketDenom,
		Credits:     []*basket.BasketCredit{{Amount: creditsToDeposit.String(), BatchDenom: batchDenom}},
	})
	require.NoError(err)

	// take 100 tokens out of the basket
	tRes, err := s.basketServer.Take(s.ctx, &basket.MsgTake{
		Owner:       user.String(),
		BasketDenom: basketDenom,
		Amount:      "100",
	})
	require.NoError(err)
	require.Len(tRes.Credits, 1)

	retireRes, err := s.msgClient.Retire(s.ctx, &core.MsgRetire{
		Holder: user.String(),
		Credits: []*core.MsgRetire_RetireCredits{
			{
				BatchDenom: batchDenom,
				Amount:     "123",
			},
		},
		Jurisdiction: "AQ",
	})
	// take.go:216:
	// Error Trace:    take.go:216
	// Error:          Received unexpected error:
	//				999999999999997.000000000100 exceeds maximum decimal places: 6
	// Test:           TestTakeFromBasket/TestBasketScenario
	s.Require().NoError(err)
	s.Require().NotNil(retireRes)
}

func (s *TakeTestSuite) fundAccount(addr sdk.AccAddress, amounts sdk.Coins) {
	err := s.bankKeeper.MintCoins(s.sdkCtx, minttypes.ModuleName, amounts)
	s.Require().NoError(err)
	err = s.bankKeeper.SendCoinsFromModuleToAccount(s.sdkCtx, minttypes.ModuleName, addr, amounts)
	s.Require().NoError(err)
}

func (s *TakeTestSuite) createClassAndIssueBatch(admin, recipient sdk.AccAddress, creditTypeAbbrev, tradableAmount, startStr, endStr string) (string, string) {
	require := s.Require()
	// fund the account so this doesn't fail
	s.fundAccount(admin, sdk.NewCoins(sdk.NewInt64Coin(sdk.DefaultBondDenom, 20000000)))

	cRes, err := s.msgClient.CreateClass(s.ctx, &core.MsgCreateClass{
		Admin:            admin.String(),
		Issuers:          []string{admin.String()},
		Metadata:         "",
		CreditTypeAbbrev: creditTypeAbbrev,
		Fee:              &createClassFee,
	})
	require.NoError(err)
	classId := cRes.ClassId
	start, err := time.Parse("2006-04-02", startStr)
	require.NoError(err)
	end, err := time.Parse("2006-04-02", endStr)
	require.NoError(err)
	pRes, err := s.msgClient.CreateProject(s.ctx, &core.MsgCreateProject{
		Issuer:       admin.String(),
		ClassId:      classId,
		Metadata:     "",
		Jurisdiction: "US-NY",
	})
	require.NoError(err)
	bRes, err := s.msgClient.CreateBatch(s.ctx, &core.MsgCreateBatch{
		Issuer:    admin.String(),
		ProjectId: pRes.ProjectId,
		Issuance:  []*core.BatchIssuance{{Recipient: recipient.String(), TradableAmount: tradableAmount}},
		Metadata:  "",
		StartDate: &start,
		EndDate:   &end,
	})
	require.NoError(err)
	batchDenom := bRes.BatchDenom
	return classId, batchDenom
}

func (s *TakeTestSuite) getUserBalance(addr sdk.AccAddress, denom string) sdk.Coin {
	require := s.Require()
	bRes, err := s.bankKeeper.Balance(s.ctx, &banktypes.QueryBalanceRequest{
		Address: addr.String(),
		Denom:   denom,
	})
	require.NoError(err)
	return *bRes.Balance
}
