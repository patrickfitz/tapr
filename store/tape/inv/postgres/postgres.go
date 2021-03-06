// Copyright 2018 Klaus Birkelund Abildgaard Jensen
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package postgres implements a PostgreSQL backed inv.Inventory.
package postgres // import "tapr.space/store/tape/inv/postgres"

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq" // side-effect: register postgresql driver

	"tapr.space"
	"tapr.space/bitmask"
	"tapr.space/errors"
	"tapr.space/log"
	"tapr.space/store/tape"
	"tapr.space/store/tape/changer"
	"tapr.space/store/tape/inv"
)

func init() {
	inv.Register("postgres", New)
}

func rollback(op string, tx *sqlx.Tx, err error) error {
	log.Error.Printf("%s: transaction roll back due to error: %v", op, err)
	if err := tx.Rollback(); err != nil {
		log.Error.Printf("%s: could not roll back transaction: %v", op, err)
	}

	return err
}

func commit(op string, tx *sqlx.Tx) error {
	if err := tx.Commit(); err != nil {
		log.Error.Printf("%s: could not commit: %v", op, err)
		return err
	}

	return nil
}

type rvol struct {
	Serial   tape.Serial         `db:"serial"`
	Location tape.Location       `db:"location"`
	Home     tape.Location       `db:"home"`
	Category tape.VolumeCategory `db:"category"`
	Flags    uint32              `db:"flags"`
}

type postgres struct {
	db *sqlx.DB

	mu sync.Mutex

	prefixCleaning string
}

var _ inv.Inventory = (*postgres)(nil)

// New returns a new postgres-backed inventory implementation.
func New(opts map[string]string) (inv.Inventory, error) {
	const op = "inv/postgres.New"

	requiredOpts := []string{
		"dbhost", "dbname", "username", "password", "cleaning-prefix",
	}

	for _, opt := range requiredOpts {
		if _, ok := opts[opt]; !ok {
			return nil, errors.E(op, errors.Strf("the %s option must be specified", opt))
		}
	}

	dsn := fmt.Sprintf("host=%s dbname=%s user=%s password=%s sslmode=disable",
		opts["dbhost"], opts["dbname"], opts["username"], opts["password"],
	)

	db, err := sqlx.Connect("postgres", dsn)
	if err != nil {
		return nil, err
	}

	return &postgres{
		db:             db,
		prefixCleaning: opts["cleaning-prefix"],
	}, nil
}

func (p *postgres) Volumes() (vs []tape.Volume, err error) {
	var rs []rvol

	err = p.db.Select(&rs, `
		SELECT serial, location, home, category, flags
		FROM volumes
		ORDER BY serial
	`)

	if err != nil {
		return
	}

	for _, r := range rs {
		vs = append(vs, tape.Volume{
			Serial:   r.Serial,
			Location: r.Location,
			Home:     r.Home,
			Category: r.Category,
			Flags:    r.Flags,
		})
	}

	return
}

func (p *postgres) Audit(chgr changer.Changer) (err error) {
	slots, err := chgr.Status()
	if err != nil {
		return err
	}

	for _, t := range tape.SlotCategories {
		var flags uint32
		if t == tape.TransferSlot {
			bitmask.Set(&flags, tape.StatusMounted)
		}

		for _, slot := range slots[t] {
			if v := slot.Volume; v != nil {
				category := tape.Scratch
				if strings.HasPrefix(string(v.Serial), p.prefixCleaning) {
					category = tape.Cleaning
				}

				_, err = p.db.Exec(`
					INSERT INTO volumes (
            			serial, location, category, flags
          			) VALUES (
						$1,       -- serial
						($2, $3), -- location (addr, slot type)
						$4,       -- category
						$5        -- flags
          			) ON CONFLICT (serial) DO
						UPDATE SET
							location = ($2, $3),
							flags = $5
				`, v.Serial, slot.Addr, slot.Category, category, fmt.Sprintf("%b", flags))

				if err != nil {
					return
				}
			}
		}
	}

	return
}

func (p *postgres) Create(path tapr.PathName, serial string) (err error) {
	const op = "inv/postgres.Create"

	stmt := `
		INSERT INTO tree (path, serial)
		VALUES ($1, $2)
	`

	if _, err = p.db.Exec(stmt, path, serial); err != nil {
		return err
	}

	return nil
}

func (p *postgres) Lookup(path tapr.PathName) (tape.Volume, error) {
	const op = "inv/postgres.Lookup"

	var r rvol

	stmt := `
		SELECT volume
		FROM tree
		JOIN
		WHERE path = $1
	`

	if err := p.db.Get(&r, stmt, path); err != nil {
		return tape.Volume{}, err
	}

	return tape.Volume{
		Serial:   r.Serial,
		Location: r.Location,
		Home:     r.Home,
		Category: r.Category,
		Flags:    r.Flags,
	}, nil
}

func (p *postgres) Load(serial tape.Serial, dst tape.Location, chgr changer.Changer) error {
	const op = "inv/postgres.Load"

	var r rvol

	tx, err := p.db.Beginx()
	if err != nil {
		return err
	}

	stmt := `
		SELECT serial, location, home, category, flags
		FROM volumes
		WHERE serial = $1
		FOR UPDATE
	`

	if err := tx.Get(&r, stmt, serial); err != nil {
		return rollback(op, tx, err)
	}

	if r.Location.Category != tape.StorageSlot && r.Location.Category != tape.ImportExportSlot {
		return errors.E(op, errors.Strf("invalid source slot for load operation"))
	}

	if dst.Category != tape.TransferSlot {
		return errors.E(op, errors.Strf("invalid destination slot for load operation"))
	}

	bitmask.Set(&r.Flags, tape.StatusTransfering)
	bitmask.Set(&r.Flags, tape.StatusMounted)

	stmt = `
		UPDATE volumes
		SET
			location = NULL,
			home = ($1, $2),
			flags = $3
		WHERE serial = $4
	`

	_, err = tx.Exec(stmt, r.Location.Addr, r.Location.Category, fmt.Sprintf("%b", r.Flags), r.Serial)
	if err != nil {
		return rollback(op, tx, err)
	}

	if err := commit(op, tx); err != nil {
		return err
	}

	if err := chgr.Load(r.Location, dst); err != nil {
		return err
	}

	bitmask.Clear(&r.Flags, tape.StatusTransfering)

	stmt = `
		UPDATE volumes
		SET
			location = ($1, $2),
			category = $3,
			flags = $4
		WHERE serial = $5
	`

	if r.Category == tape.Allocating {
		r.Category = tape.Allocated
	}

	if _, err := p.db.Exec(stmt, dst.Addr, dst.Category, r.Category, fmt.Sprintf("%b", r.Flags), r.Serial); err != nil {
		return err
	}

	return nil
}

func (p *postgres) Unload(serial tape.Serial, dst tape.Location, chgr changer.Changer) error {
	const op = "inv/postgres.Unload"

	var r rvol

	tx, err := p.db.Beginx()
	if err != nil {
		return err
	}

	stmt := `
		SELECT serial, location, home, category, flags
		FROM volumes
		WHERE serial = $1
	`

	if err := tx.Get(&r, stmt, serial); err != nil {
		return rollback(op, tx, err)
	}

	if dst.Addr == 0 {
		// return to home slot
		dst = r.Home
	}

	if r.Location.Category != tape.TransferSlot {
		return errors.E(op, errors.Strf("invalid source slot for unload operation"))
	}

	if dst.Category != tape.StorageSlot && dst.Category != tape.ImportExportSlot {
		return errors.E(op, errors.Strf("invalid destination slot for unload operation"))
	}

	bitmask.Clear(&r.Flags, tape.StatusMounted)
	bitmask.Set(&r.Flags, tape.StatusTransfering)

	stmt = `
		UPDATE volumes
		SET
			location = NULL,
			flags = $1
		WHERE serial = $2
	`

	_, err = tx.Exec(stmt, fmt.Sprintf("%b", r.Flags), r.Serial)
	if err != nil {
		return rollback(op, tx, err)
	}

	if err := commit(op, tx); err != nil {
		return err
	}

	if err := chgr.Unload(r.Location, dst); err != nil {
		return err
	}

	bitmask.Clear(&r.Flags, tape.StatusTransfering)

	stmt = `
		UPDATE volumes
		SET
			location = $1,
			home = NULL,
			flags = $2
		WHERE serial = $3
	`

	if _, err := tx.Exec(stmt, r.Flags, r.Serial); err != nil {
		return err
	}

	return nil
}

func (p *postgres) Transfer(serial tape.Serial, dst tape.Location, chgr changer.Changer) error {
	const op = "inv/postgres.Transfer"

	var r rvol

	tx, err := p.db.Beginx()
	if err != nil {
		return err
	}

	stmt := `
		SELECT serial, location, home, category, flags
		FROM volumes
		WHERE serial = $1
	`

	if err := tx.Get(&r, stmt, serial); err != nil {
		return rollback(op, tx, err)
	}

	if r.Location.Category != tape.StorageSlot && r.Location.Category != tape.ImportExportSlot {
		return errors.E(op, errors.Strf("invalid source slot for transfer operation"))
	}

	if dst.Category != tape.StorageSlot && dst.Category != tape.ImportExportSlot {
		return errors.E(op, errors.Strf("invalid destination slot for transfer"))
	}

	// set transfering flag
	bitmask.Set(&r.Flags, tape.StatusTransfering)

	stmt = `
		UPDATE volumes
		SET
			location = NULL,
			flags = $3
		WHERE serial = $4
	`

	_, err = tx.Exec(stmt, r.Location, r.Flags, r.Serial)
	if err != nil {
		return rollback(op, tx, err)
	}

	if err := commit(op, tx); err != nil {
		return err
	}

	if err := chgr.Transfer(r.Location, dst); err != nil {
		return err
	}

	bitmask.Clear(&r.Flags, tape.StatusTransfering)

	stmt = `
		UPDATE volumes
		SET
			location = $1,
			flags = $2
		WHERE serial = $3
	`

	if _, err := tx.Exec(stmt, dst, r.Flags, r.Serial); err != nil {
		return err
	}

	return nil
}

func (p *postgres) Loaded(loc tape.Location) (loaded bool, serial tape.Serial, err error) {
	const op = "inv/postgres.Loaded"

	err = p.db.Get(&serial, `
		SELECT serial
		FROM volumes
		WHERE
			location = ($1::integer,slot_category('transfer'))
	`, loc.Addr)

	if err != nil {
		if err == sql.ErrNoRows {
			return false, serial, nil
		}

		log.Error.Print(loc)
		return false, serial, errors.E(op, err)
	}

	return true, serial, nil
}

func (p *postgres) Info(serial tape.Serial) (tape.Volume, error) {
	const op = "inv/postgres.Info"

	var r rvol

	err := p.db.Get(&r, `
		SELECT serial, location, home, category, flags
		FROM volumes
		WHERE serial = $1
	`, serial)

	if err != nil {
		return tape.Volume{}, err
	}

	return tape.Volume{
		Serial:   r.Serial,
		Location: r.Location,
		Home:     r.Home,
		Category: r.Category,
		Flags:    r.Flags,
	}, nil
}

func (p *postgres) Update(vol tape.Volume) error {
	const op = "inv/postgres.Update"

	stmt := `
		UPDATE volumes
		SET
			location = ($1, $2),
			home = ($3, $4),
			category = $5,
			flags = $6
		WHERE serial = $7
	`

	_, err := p.db.Exec(stmt,
		vol.Location.Addr, vol.Location.Category,
		vol.Home.Addr, vol.Home.Category,
		vol.Category, fmt.Sprintf("%b", vol.Flags),
		vol.Serial,
	)
	if err != nil {
		return err
	}

	return nil
}

func (p *postgres) Alloc() (serial tape.Serial, err error) {
	const op = "inv/postgres.Alloc"

	tx, err := p.db.Beginx()
	if err != nil {
		return serial, err
	}

	var r rvol

	stmt := `
		SELECT serial, location, home, category, flags
		FROM volumes
		WHERE category IN ('filling', 'scratch')
		  AND (location).category = 'storage'
		ORDER BY category, serial
		LIMIT 1
		FOR UPDATE
	`

	if err := tx.Get(&r, stmt); err != nil {
		return serial, rollback(op, tx, err)
	}

	serial = r.Serial

	if r.Category != tape.Filling {
		r.Category = tape.Allocating

		stmt = `
			UPDATE volumes
			SET category = $1
			WHERE serial = $2
		`

		if _, err = tx.Exec(stmt, r.Category, r.Serial); err != nil {
			return serial, rollback(op, tx, err)
		}
	}

	if err := commit(op, tx); err != nil {
		return serial, err
	}

	return serial, nil
}

// Reset resets the inventory database.
func (p *postgres) Reset() error {
	for _, stmt := range resetSchema {
		if _, err := p.db.Exec(stmt); err != nil {
			return err
		}
	}

	return nil
}
