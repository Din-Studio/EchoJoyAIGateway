package dbtx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Mode selects the isolation level a transaction begins with.
type Mode uint8

const (
	Write Mode = iota
	ReadSnapshot
)

// Phase identifies the infrastructure operation that failed.
type Phase string

const (
	PhaseInput      Phase = "validate transaction"
	PhaseConnection Phase = "pin database connection"
	PhaseBegin      Phase = "begin transaction"
	PhaseCommit     Phase = "commit transaction"
	PhaseRollback   Phase = "rollback transaction"
	PhaseDiscard    Phase = "discard database connection"
)

// Error represents a transaction infrastructure failure. Callback errors are
// returned directly so callers can preserve their business error semantics.
type Error struct {
	Operation string
	Phase     Phase
	Err       error
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	label := string(err.Phase)
	if err.Operation != "" {
		label = err.Operation + ": " + label
	}
	if err.Err == nil {
		return label
	}
	return fmt.Sprintf("%s: %v", label, err.Err)
}

func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// IsInfrastructure reports whether an error contains a transaction
// infrastructure failure, including a cleanup failure joined with a callback
// error.
func IsInfrastructure(err error) bool {
	var target *Error
	return errors.As(err, &target)
}

type Options struct {
	Mode           Mode
	CleanupTimeout time.Duration
	Operation      string
}

// Run executes callback inside a pinned SQL connection and a PostgreSQL
// transaction. A failed callback is rolled back; a failed rollback or commit
// causes the connection to be discarded so it cannot return to the pool in an
// unknown transaction state.
func Run(
	ctx context.Context,
	db *gorm.DB,
	options Options,
	callback func(*gorm.DB) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if db == nil {
		return newError(options.Operation, PhaseInput, errors.New("database is nil"))
	}
	if callback == nil {
		return newError(options.Operation, PhaseInput, errors.New("transaction callback is nil"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	beginStatement, err := beginStatementFor(options.Mode)
	if err != nil {
		return withOperation(err, options.Operation)
	}
	cleanupTimeout := options.CleanupTimeout
	if cleanupTimeout <= 0 {
		cleanupTimeout = time.Second
	}

	return db.WithContext(ctx).Connection(func(connection *gorm.DB) error {
		sqlConn, ok := connection.Statement.ConnPool.(*sql.Conn)
		if !ok {
			return newError(
				options.Operation,
				PhaseConnection,
				fmt.Errorf("expected *sql.Conn, got %T", connection.Statement.ConnPool),
			)
		}

		if _, err := sqlConn.ExecContext(ctx, beginStatement); err != nil {
			cleanupErr := discardBadConnection(options.Operation, sqlConn, err)
			return errors.Join(newError(options.Operation, PhaseBegin, err), cleanupErr)
		}

		transaction := connection.Session(&gorm.Session{
			NewDB: true, SkipDefaultTransaction: true, Context: ctx,
		})
		active := true
		defer func() {
			if active {
				_ = rollback(options.Operation, sqlConn, cleanupTimeout, false)
			}
		}()

		if err := callback(transaction); err != nil {
			cleanupErr := rollback(options.Operation, sqlConn, cleanupTimeout, false)
			active = false
			if options.Mode == ReadSnapshot {
				if parentErr := ctx.Err(); parentErr != nil {
					return errors.Join(parentErr, cleanupErr)
				}
			}
			return errors.Join(err, cleanupErr)
		}
		if options.Mode == ReadSnapshot {
			if parentErr := ctx.Err(); parentErr != nil {
				cleanupErr := rollback(options.Operation, sqlConn, cleanupTimeout, false)
				active = false
				return errors.Join(parentErr, cleanupErr)
			}
		}

		if _, err := sqlConn.ExecContext(ctx, "COMMIT"); err != nil {
			commitErr := newError(options.Operation, PhaseCommit, err)
			cleanupErr := rollback(options.Operation, sqlConn, cleanupTimeout, true)
			active = false
			if options.Mode == ReadSnapshot {
				if parentErr := ctx.Err(); parentErr != nil {
					return errors.Join(parentErr, cleanupErr)
				}
			}
			return errors.Join(commitErr, cleanupErr)
		}
		active = false
		return nil
	})
}

// beginStatementFor returns the PostgreSQL BEGIN statement for mode.
//
// Write transactions pin READ COMMITTED explicitly: the incremental
// ON CONFLICT DO UPDATE upserts in requestlog rely on PostgreSQL re-evaluating
// the assignment against the latest committed row, and a server whose
// default_transaction_isolation was raised would otherwise turn concurrent
// writers into serialization failures. Read snapshots use REPEATABLE READ so
// every read in a report sees one stable snapshot.
func beginStatementFor(mode Mode) (string, error) {
	switch mode {
	case Write:
		return "BEGIN ISOLATION LEVEL READ COMMITTED", nil
	case ReadSnapshot:
		return "BEGIN ISOLATION LEVEL REPEATABLE READ", nil
	default:
		return "", &Error{
			Phase: PhaseInput,
			Err:   fmt.Errorf("unsupported transaction mode %d", mode),
		}
	}
}

func rollback(
	operation string,
	sqlConn *sql.Conn,
	cleanupTimeout time.Duration,
	discardAlways bool,
) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, rollbackErr := sqlConn.ExecContext(cleanupCtx, "ROLLBACK")

	var discardErr error
	if rollbackErr != nil || discardAlways {
		discardErr = discardConnection(operation, sqlConn)
	}
	var result []error
	if rollbackErr != nil {
		result = append(result, newError(operation, PhaseRollback, rollbackErr))
	}
	if discardErr != nil {
		result = append(result, discardErr)
	}
	return errors.Join(result...)
}

func discardBadConnection(
	operation string,
	sqlConn *sql.Conn,
	err error,
) error {
	if !errors.Is(err, driver.ErrBadConn) {
		return nil
	}
	return discardConnection(operation, sqlConn)
}

func discardConnection(operation string, sqlConn *sql.Conn) error {
	err := sqlConn.Raw(func(any) error { return driver.ErrBadConn })
	if err == nil || errors.Is(err, driver.ErrBadConn) {
		return nil
	}
	return newError(operation, PhaseDiscard, err)
}

func newError(operation string, phase Phase, err error) error {
	return &Error{Operation: operation, Phase: phase, Err: err}
}

func withOperation(err error, operation string) error {
	var transactionErr *Error
	if !errors.As(err, &transactionErr) || transactionErr.Operation != "" {
		return err
	}
	transactionErr.Operation = operation
	return err
}
