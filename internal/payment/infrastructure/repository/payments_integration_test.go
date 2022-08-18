package repository

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/log/zerologadapter"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/moeryomenko/saga/internal/payment/domain"
)

func TestIntegration_Payments(t *testing.T) {
	config, err := pgxpool.ParseConfig(`user=test password=pass host=localhost port=5432 dbname=payments pool_max_conns=1`)
	require.NoError(t, err)

	zlog := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr})
	config.ConnConfig.Logger = zerologadapter.NewLogger(zlog)

	pool, err = pgxpool.ConnectConfig(context.Background(), config)
	require.NoError(t, err)
	defer func() {
		pool.Close()
	}()

	positivePayments(context.Background(), t)
	negativePayments(context.Background(), t)
}

func positivePayments(ctx context.Context, t *testing.T) {
	testcases := map[string]struct {
		orderID                uuid.UUID
		customer               func() (uuid.UUID, error)
		amount                 decimal.Decimal
		finalEvent             func(uuid.UUID) domain.Event
		expectedCreatedBalance domain.Balance
		expectedFinalBalance   domain.Balance
	}{
		`completed payments`: {
			orderID: uuid.New(),
			customer: func() (uuid.UUID, error) {
				customerID := uuid.New()
				available := decimal.NewFromInt32(100)
				err := pool.BeginTxFunc(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO balances(customer_id, available_amount) VALUES ($1, $2)`, customerID, available)
					return err
				})
				return customerID, err
			},
			amount: decimal.NewFromInt32(20),
			finalEvent: func(u uuid.UUID) domain.Event {
				return domain.Complete{PaymentID: u}
			},
			expectedCreatedBalance: domain.Balance{
				Amount:   decimal.NewFromInt32(80),
				Reserved: decimal.NewFromInt32(20),
			},
			expectedFinalBalance: domain.Balance{
				Amount:   decimal.NewFromInt32(80),
				Reserved: decimal.Zero,
			},
		},
		`canceled payments`: {
			orderID: uuid.New(),
			customer: func() (uuid.UUID, error) {
				customerID := uuid.New()
				available := decimal.NewFromInt32(100)
				err := pool.BeginTxFunc(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO balances(customer_id, available_amount) VALUES ($1, $2)`, customerID, available)
					return err
				})
				return customerID, err
			},
			amount: decimal.NewFromInt32(20),
			finalEvent: func(u uuid.UUID) domain.Event {
				return domain.Cancel{PaymentID: u}
			},
			expectedCreatedBalance: domain.Balance{
				Amount:   decimal.NewFromInt32(80),
				Reserved: decimal.NewFromInt32(20),
			},
			expectedFinalBalance: domain.Balance{
				Amount:   decimal.NewFromInt32(100),
				Reserved: decimal.Zero,
			},
		},
	}

	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			customerID, err := tc.customer()
			require.NoError(t, err)
			tc.expectedCreatedBalance.CustomerID = customerID
			tc.expectedFinalBalance.CustomerID = customerID

			// create payments.
			payment, err := PersistTransaction(ctx, customerID, domain.Reserve{OrderID: tc.orderID, Amount: tc.amount})
			require.NoError(t, err)
			checkBalance(ctx, t, customerID, tc.expectedCreatedBalance)
			if _, ok := payment.(domain.NewPayment); !ok {
				require.Fail(t, `expected payment created`)
			}

			// complete payments.
			event := tc.finalEvent(payment.GetID())
			_, err = PersistTransaction(ctx, customerID, event)
			require.NoError(t, err)
			checkBalance(ctx, t, customerID, tc.expectedFinalBalance)
		})
	}
}

func negativePayments(ctx context.Context, t *testing.T) {
	testcases := map[string]struct {
		orderID         uuid.UUID
		customer        func() (uuid.UUID, error)
		preparePayment  func(uuid.UUID, uuid.UUID) (uuid.UUID, error)
		event           func(uuid.UUID, uuid.UUID) domain.Event
		expectedError   error
		expectedBalance domain.Balance
	}{
		`insufficient funds`: {
			orderID: uuid.New(),
			customer: func() (uuid.UUID, error) {
				customerID := uuid.New()
				available := decimal.NewFromInt32(40)
				err := pool.BeginTxFunc(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO balances(customer_id, available_amount) VALUES ($1, $2)`, customerID, available)
					return err
				})
				return customerID, err
			},
			event: func(orderID, _ uuid.UUID) domain.Event {
				return domain.Reserve{OrderID: orderID, Amount: decimal.NewFromInt32(50)}
			},
			expectedBalance: domain.Balance{
				Amount:   decimal.NewFromInt32(40),
				Reserved: decimal.Zero,
			},
			expectedError: domain.ErrInsufficientFunds,
		},
		`cancel completed payments`: {
			orderID: uuid.New(),
			customer: func() (uuid.UUID, error) {
				customerID := uuid.New()
				available := decimal.NewFromInt32(40)
				err := pool.BeginTxFunc(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO balances(customer_id, available_amount) VALUES ($1, $2)`, customerID, available)
					return err
				})
				return customerID, err
			},
			preparePayment: func(customerID, orderID uuid.UUID) (uuid.UUID, error) {
				paymentID := uuid.New()
				err := pool.BeginTxFunc(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, insertPaymentQuery, paymentID, statusCompleted, customerID, orderID, decimal.NewFromInt32(20))
					return err
				})
				return paymentID, err
			},
			event: func(_, paymentID uuid.UUID) domain.Event {
				return domain.Cancel{PaymentID: paymentID}
			},
			expectedBalance: domain.Balance{
				Amount:   decimal.NewFromInt32(40),
				Reserved: decimal.Zero,
			},
			expectedError: domain.ErrCompletedPayment,
		},
		`complete canceled payments`: {
			orderID: uuid.New(),
			customer: func() (uuid.UUID, error) {
				customerID := uuid.New()
				available := decimal.NewFromInt32(40)
				err := pool.BeginTxFunc(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO balances(customer_id, available_amount) VALUES ($1, $2)`, customerID, available)
					return err
				})
				return customerID, err
			},
			preparePayment: func(customerID, orderID uuid.UUID) (uuid.UUID, error) {
				paymentID := uuid.New()
				err := pool.BeginTxFunc(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, insertPaymentQuery, paymentID, statusCanceled, customerID, orderID, decimal.NewFromInt32(20))
					return err
				})
				return paymentID, err
			},
			event: func(_, paymentID uuid.UUID) domain.Event {
				return domain.Complete{PaymentID: paymentID}
			},
			expectedBalance: domain.Balance{
				Amount:   decimal.NewFromInt32(40),
				Reserved: decimal.Zero,
			},
			expectedError: domain.ErrCanceledPayment,
		},
	}

	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			customerID, err := tc.customer()
			require.NoError(t, err)

			paymentID, err := func() (uuid.UUID, error) {
				if tc.preparePayment != nil {
					return tc.preparePayment(customerID, tc.orderID)
				}
				return uuid.UUID{}, nil
			}()
			require.NoError(t, err)

			_, err = PersistTransaction(ctx, customerID, tc.event(tc.orderID, paymentID))
			require.ErrorIs(t, err, tc.expectedError)
			checkBalance(ctx, t, customerID, tc.expectedBalance)
		})
	}
}

func checkBalance(ctx context.Context, t *testing.T, customerID uuid.UUID, expectedBalance domain.Balance) {
	var balance domain.Balance
	pool.BeginTxFunc(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) (err error) {
		balance, err = findBalanceByCustomer(ctx, tx, customerID)
		return
	})
	require.Equal(t, expectedBalance.Amount.String(), balance.Amount.String())
	require.Equal(t, expectedBalance.Reserved.String(), balance.Reserved.String())
}
