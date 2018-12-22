// +build !oss

/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Dgraph Community License (the "License"); you
 * may not use this file except in compliance with the License. You
 * may obtain a copy of the License at
 *
 *     https://github.com/dgraph-io/dgraph/blob/master/licenses/DCL.txt
 */

package restore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/dgraph-io/badger/options"
	"github.com/dgraph-io/dgraph/ee/backup"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/x"
	"github.com/spf13/cobra"
)

var Restore x.SubCommand

var opt struct {
	location, pdir string
	progress       bool
}

func init() {
	Restore.Cmd = &cobra.Command{
		Use:   "restore",
		Short: "Run Dgraph (EE) Restore backup",
		Long: `
		Dgraph Restore is used to load backup files offline.
		`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			defer x.StartProfile(Restore.Conf).Stop()
			if err := run(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		},
	}

	flag := Restore.Cmd.Flags()
	flag.StringVarP(&opt.location, "location", "l", "",
		"Sets the source location URI (required).")
	flag.StringVarP(&opt.pdir, "postings", "p", "",
		"Directory where posting lists are stored (required).")
	flag.BoolVar(&opt.progress, "progress", false,
		"Enable show detailed progress.")
	_ = Restore.Cmd.MarkFlagRequired("postings")
	_ = Restore.Cmd.MarkFlagRequired("location")
}

func run() error {
	fmt.Println("Restoring backups from:", opt.location)
	fmt.Println("Writing postings to:", opt.pdir)

	bo := badger.DefaultOptions
	bo.SyncWrites = false
	bo.TableLoadingMode = options.MemoryMap
	bo.ValueThreshold = 1 << 10
	bo.NumVersionsToKeep = math.MaxInt32

	// num is used to create the posting 'p*' directories for each group.
	var num int

	// Scan location for backup files and load them.
	return backup.Load(opt.location, func(reader io.Reader, name string) error {
		var (
			kvs pb.KVS       // KV process queue
			bb  bytes.Buffer // KV read buffer
			sz  uint64       // size of KV value
			cnt int          // total count of KV records loaded
		)

		bo := bo
		bo.Dir = filepath.Join(opt.pdir, fmt.Sprintf("p%d", num)) // p0 ... pN-1
		bo.ValueDir = bo.Dir
		db, err := badger.OpenManaged(bo)
		if err != nil {
			return err
		}
		defer db.Close()
		fmt.Println("--- Creating new db:", bo.Dir)
		fmt.Println("--- Loading:", name)

		writer := x.NewTxnWriter(db)
		writer.BlindWrite = true

		kvs.Kv = make([]*pb.KV, 0, 1000)
		start := time.Now()

		// start progress ticker
		tick := time.NewTicker(time.Second)
		done := make(chan struct{})
		if opt.progress {
			go func() {
				for {
					select {
					case <-done:
						return
					case now := <-tick.C:
						fmt.Printf("... Time elapsed: %s, keys loaded: %d, speed: %d keys/s\n",
							now.Sub(start).Round(time.Second), cnt,
							int64(float64(cnt)/time.Since(start).Seconds()))
					}
				}
			}()
		}

		// This loop will access reader until EOF (or an error) is returned.
		for {
			err = binary.Read(reader, binary.LittleEndian, &sz)
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}

			buf := make([]byte, int(sz))
			n, err := io.ReadFull(reader, buf)
			if err != nil {
				return err
			}
			// The byte count must match the header otherwise we have data loss.
			if n != int(sz) {
				return x.Errorf("Restore failed read. Expected %d bytes but got %d instead.", sz, n)
			}
			e := &pb.KV{}
			if err = e.Unmarshal(bb.Bytes()); err != nil {
				return err
			}
			bb.Reset()
			kvs.Kv = append(kvs.Kv, e)
			kvs.Done = false
			cnt++

			// check if KV queue is full, then send.
			if cnt%1000 == 0 {
				if err = writer.Send(&kvs); err != nil {
					return err
				}
				kvs.Kv = make([]*pb.KV, 0, 1000)
				kvs.Done = true
			}
		}
		// check if we have more unsent queued KV's.
		if !kvs.Done {
			if err := writer.Send(&kvs); err != nil {
				return err
			}
		}
		if err := writer.Flush(); err != nil {
			return err
		}

		// increment to next pN dir for a new DB.
		num++

		// stop progress ticker
		tick.Stop()
		done <- struct{}{}

		fmt.Printf("--- Loaded %d keys in %s\n", cnt, time.Since(start).Round(time.Second))

		return nil
	})
}
