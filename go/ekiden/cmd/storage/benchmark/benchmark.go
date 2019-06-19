// Package benchmark implements the storage benchmark sub-command.
package benchmark

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/oasislabs/ekiden/go/common"
	"github.com/oasislabs/ekiden/go/common/crypto/hash"
	memorySigner "github.com/oasislabs/ekiden/go/common/crypto/signature/signers/memory"
	"github.com/oasislabs/ekiden/go/common/identity"
	"github.com/oasislabs/ekiden/go/common/logging"
	cmdCommon "github.com/oasislabs/ekiden/go/ekiden/cmd/common"
	"github.com/oasislabs/ekiden/go/storage"
	storageAPI "github.com/oasislabs/ekiden/go/storage/api"
)

const (
	cfgProfileCPU = "benchmark.profile_cpu"
	cfgProfileMEM = "benchmark.profile_mem"
)

var (
	benchmarkStorageCmd = &cobra.Command{
		Use:   "benchmark",
		Short: "benchmark storage backend",
		Run:   doBenchmark,
	}
)

func doBenchmark(cmd *cobra.Command, args []string) { // nolint: gocyclo
	// Re-register flags due to https://github.com/spf13/viper/issues/233.
	RegisterFlags(cmd)

	if err := cmdCommon.Init(); err != nil {
		cmdCommon.EarlyLogAndExit(err)
	}

	logger := logging.GetLogger("cmd/storage/benchmark")

	var err error

	// Initialize the data directory.
	dataDir := cmdCommon.DataDir()
	if dataDir == "" {
		dataDir, err = ioutil.TempDir("", "storage-benchmark")
		if err != nil {
			logger.Error("failed to initialize data directory",
				"err", err,
			)
			return
		}

		logger.Debug("using temporary data directory",
			"data_dir", dataDir,
		)

		defer os.RemoveAll(dataDir)
	}

	// Create an identity.
	ident, err := identity.LoadOrGenerate(dataDir, memorySigner.NewFactory())
	if err != nil {
		logger.Error("failed to generate a new identity",
			"err", err,
		)
		return
	}

	// Disable expected root checks.
	viper.Set("storage.debug.insecure_skip_checks", true)

	storage, err := storage.New(context.Background(), dataDir, ident, nil, nil)
	if err != nil {
		logger.Error("failed to initialize storage",
			"err", err,
		)
		return
	}
	defer storage.Cleanup()

	// Wait for storage initialization.
	<-storage.Initialized()

	if viper.GetBool(cfgProfileCPU) {
		// Enable CPU profiling.
		prof, perr := os.Create("storage-bench-profile.prof")
		if perr != nil {
			logger.Error("failed to create file for CPU profiler output",
				"err", perr,
			)
			return
		}
		defer prof.Close()
		if perr = pprof.StartCPUProfile(prof); perr != nil {
			logger.Error("failed to start CPU profiler",
				"err", perr,
			)
			return
		}
		defer pprof.StopCPUProfile()
	}

	var ns common.Namespace

	// Benchmark MKVS storage (single-insert).
	for _, sz := range []int{
		256, 512, 1024, 4096, 8192, 16384, 32768,
	} {
		buf := make([]byte, sz)
		key := []byte(strconv.Itoa(sz))

		// This will store the new Urkel tree root for later lookups.
		var newRoot storageAPI.Root
		newRoot.Namespace = ns
		newRoot.Round = 1
		newRoot.Hash.Empty()

		// Apply.
		res := testing.Benchmark(func(b *testing.B) {
			b.SetBytes(int64(sz))
			var root, unknown hash.Hash
			root.Empty()
			// We don't want to optimize-away Apply ops, so give a bogus expected root.
			unknown.FromBytes([]byte("Unknown new root"))
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				_, _ = io.ReadFull(rand.Reader, buf)
				wl := storageAPI.WriteLog{storageAPI.LogEntry{Key: key, Value: buf}}
				b.StartTimer()

				var receipts []*storageAPI.Receipt
				receipts, err = storage.Apply(context.Background(), ns, 0, root, 1, unknown, wl)
				if err != nil {
					b.Fatalf("failed to Apply(): %v", err)
				}

				// Open the first receipt and obtain the new root from it.
				b.StopTimer()
				var receiptBody storageAPI.ReceiptBody
				if err = receipts[0].Open(&receiptBody); err != nil {
					b.Fatalf("failed to Open(): %v", err)
				}
				newRoot.Hash = receiptBody.Roots[0]
				b.StartTimer()
			}
		})
		if err != nil {
			logger.Error("failed to Apply()", "err", err)
		} else {
			logger.Info("Apply",
				"sz", sz,
				"ns_per_op", res.NsPerOp(),
			)
		}

		// GetSubtree.
		res = testing.Benchmark(func(b *testing.B) {
			b.SetBytes(int64(sz))
			for i := 0; i < b.N; i++ {
				_, err = storage.GetSubtree(context.Background(), newRoot, storageAPI.NodeID{Path: []byte{}, BitDepth: 0}, 10)
				if err != nil {
					b.Fatalf("failed to GetSubtree(): %v", err)
				}
			}
		})
		if err != nil {
			logger.Error("failed to GetSubtree()", "err", err)
		} else {
			logger.Info("GetSubtree",
				"sz", sz,
				"ns_per_op", res.NsPerOp(),
			)
		}
	}

	// Benchmark MKVS batch-insert.
	for _, bsz := range []int{
		1, 2, 4, 8, 16, 32,
	} {
		for _, sz := range []int{
			256, 512, 1024, 4096, 8192, 16384,
		} {
			// Apply batch.
			res := testing.Benchmark(func(b *testing.B) {
				b.SetBytes(int64(bsz * sz))
				var root, unknown hash.Hash
				root.Empty()
				// We don't want to optimize-away Apply ops, so give a bogus expected root.
				unknown.FromBytes([]byte("Unknown new root"))
				for i := 0; i < b.N; i++ {
					// Prepare batch.
					b.StopTimer()
					var wl storageAPI.WriteLog
					for j := 0; j < bsz; j++ {
						buf := make([]byte, sz)
						_, _ = io.ReadFull(rand.Reader, buf)
						key := []byte(fmt.Sprintf("bsz=%d,sz=%d,j=%d", bsz, sz, j))
						wl = append(wl, storageAPI.LogEntry{Key: key, Value: buf})
					}
					b.StartTimer()

					_, err = storage.Apply(context.Background(), ns, 0, root, 1, unknown, wl)
					if err != nil {
						b.Fatalf("failed to Apply(): %v", err)
					}
				}
			})
			if err != nil {
				logger.Error("failed to Apply()", "err", err)
			} else {
				logger.Info("Apply",
					"bsz", bsz,
					"sz", sz,
					"ns_per_op", res.NsPerOp(),
				)
			}
		}
	}

	// Benchmark concurrent MKVS Apply with same write log.
	testValues := [][]byte{
		[]byte("Thou seest Me as Time who kills, Time who brings all to doom,"),
		[]byte("The Slayer Time, Ancient of Days, come hither to consume;"),
		[]byte("Excepting thee, of all these hosts of hostile chiefs arrayed,"),
		[]byte("There shines not one shall leave alive the battlefield!"),
	}
	expectedNewRoot := [...]byte{82, 3, 202, 16, 125, 182, 175, 25, 51, 188, 131, 181, 118, 76, 249, 15, 53, 89, 59, 224, 95, 75, 239, 182, 157, 30, 80, 48, 237, 108, 90, 22}
	var emptyRoot hash.Hash
	emptyRoot.Empty()

	var wl storageAPI.WriteLog
	blen := 0
	for i, v := range testValues {
		wl = append(wl, storageAPI.LogEntry{Key: []byte(strconv.Itoa(i)), Value: v})
		blen = blen + len(v)
	}

	var cerr error
	res := testing.Benchmark(func(b *testing.B) {
		b.SetBytes(int64(blen))
		b.SetParallelism(100)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, cerr = storage.Apply(context.Background(), ns, 0, emptyRoot, 1, expectedNewRoot, wl)
				if cerr != nil {
					b.Fatalf("failed to Apply(): %v", cerr)
				}
			}
		})
	})
	if cerr != nil {
		logger.Error("failed to Apply() concurrently", "err", cerr)
	} else {
		logger.Info("ApplyConcurrently",
			"sz", blen,
			"ns_per_op", res.NsPerOp(),
		)
	}

	if viper.GetBool(cfgProfileMEM) {
		// Write memory profiling data.
		mprof, merr := os.Create("storage-bench-mem-profile.prof")
		if merr != nil {
			logger.Error("failed to create file for memory profiler output",
				"err", merr,
			)
			return
		}
		defer mprof.Close()
		runtime.GC()
		if merr = pprof.WriteHeapProfile(mprof); merr != nil {
			logger.Error("failed to write heap profile",
				"err", merr,
			)
		}
	}
}

// RegisterFlags registers the flags used by the benchmark sub-command.
func RegisterFlags(cmd *cobra.Command) {
	if !cmd.Flags().Parsed() {
		cmd.Flags().Bool(cfgProfileCPU, false, "Enable CPU profiling in benchmark")
		cmd.Flags().Bool(cfgProfileMEM, false, "Enable memory profiling in benchmark")
	}

	for _, v := range []string{
		cfgProfileCPU,
		cfgProfileMEM,
	} {
		viper.BindPFlag(v, cmd.Flags().Lookup(v)) //nolint: errcheck
	}

	for _, v := range []func(*cobra.Command){
		storage.RegisterFlags,
	} {
		v(cmd)
	}
}

// Register registers the storage benchmark sub-command.
func Register(parentCmd *cobra.Command) {
	RegisterFlags(benchmarkStorageCmd)
	parentCmd.AddCommand(benchmarkStorageCmd)
}
