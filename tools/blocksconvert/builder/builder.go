// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/tools/blocksconvert/builder/builder.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package builder

import (
	"context"
	"flag"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/backoff"
	"github.com/grafana/dskit/services"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/objstore"
	"golang.org/x/sync/errgroup"

	"github.com/grafana/mimir/pkg/chunk"
	"github.com/grafana/mimir/pkg/chunk/cache"
	"github.com/grafana/mimir/pkg/chunk/storage"
	"github.com/grafana/mimir/pkg/storage/bucket"
	mimir_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"

	"github.com/grafana/mimir/tools/blocksconvert"
	"github.com/grafana/mimir/tools/blocksconvert/planprocessor"
)

// How many series are kept in the memory before sorting and writing them to the file.
const defaultSeriesBatchSize = 250000

type Config struct {
	OutputDirectory string
	Concurrency     int

	ChunkCacheConfig   cache.Config
	UploadBlock        bool
	DeleteLocalBlock   bool
	SeriesBatchSize    int
	TimestampTolerance time.Duration

	PlanProcessorConfig planprocessor.Config
}

func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.ChunkCacheConfig.RegisterFlagsWithPrefix("chunks.", "Chunks cache", f)
	cfg.PlanProcessorConfig.RegisterFlags("builder", f)

	f.StringVar(&cfg.OutputDirectory, "builder.output-dir", "", "Local directory used for storing temporary plan files (will be created, if missing).")
	f.IntVar(&cfg.Concurrency, "builder.concurrency", 128, "Number of concurrent series processors.")
	f.BoolVar(&cfg.UploadBlock, "builder.upload", true, "Upload generated blocks to storage.")
	f.BoolVar(&cfg.DeleteLocalBlock, "builder.delete-local-blocks", true, "Delete local files after uploading block.")
	f.IntVar(&cfg.SeriesBatchSize, "builder.series-batch-size", defaultSeriesBatchSize, "Number of series to keep in memory before batch-write to temp file. Lower to decrease memory usage during the block building.")
	f.DurationVar(&cfg.TimestampTolerance, "builder.timestamp-tolerance", 0, "Adjust sample timestamps by up to this to align them to an exact number of seconds apart.")
}

func NewBuilder(cfg Config, scfg blocksconvert.SharedConfig, l log.Logger, reg prometheus.Registerer) (services.Service, error) {
	err := scfg.SchemaConfig.Load()
	if err != nil {
		return nil, errors.Wrap(err, "failed to load schema")
	}

	bucketClient, err := scfg.GetBucket(l, reg)
	if err != nil {
		return nil, err
	}

	if cfg.OutputDirectory == "" {
		return nil, errors.New("no output directory")
	}
	if err := os.MkdirAll(cfg.OutputDirectory, os.FileMode(0700)); err != nil {
		return nil, errors.Wrap(err, "failed to create output directory")
	}

	b := &Builder{
		cfg: cfg,

		bucketClient:  bucketClient,
		schemaConfig:  scfg.SchemaConfig,
		storageConfig: scfg.StorageConfig,

		fetchedChunks: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocksconvert_builder_fetched_chunks_total",
			Help: "Fetched chunks",
		}),
		fetchedChunksSize: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocksconvert_builder_fetched_chunks_bytes_total",
			Help: "Fetched chunks bytes",
		}),
		processedSeries: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocksconvert_builder_series_total",
			Help: "Processed series",
		}),
		writtenSamples: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocksconvert_builder_written_samples_total",
			Help: "Written samples",
		}),
		buildInProgress: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "cortex_blocksconvert_builder_in_progress",
			Help: "Build in progress",
		}),
		chunksNotFound: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocksconvert_builder_chunks_not_found_total",
			Help: "Number of chunks that were not found on the storage.",
		}),
		blocksSize: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: "cortex_blocksconvert_builder_block_size_bytes_total",
			Help: "Total size of blocks generated by this builder.",
		}),
		seriesInMemory: promauto.With(reg).NewGauge(prometheus.GaugeOpts{
			Name: "cortex_blocksconvert_builder_series_in_memory",
			Help: "Number of series kept in memory at the moment. (Builder writes series to temp files in order to reduce memory usage.)",
		}),
	}

	return planprocessor.NewService(cfg.PlanProcessorConfig, filepath.Join(cfg.OutputDirectory, "plans"), bucketClient, b.cleanupFn, b.planProcessorFactory, l, reg)
}

type Builder struct {
	cfg Config

	bucketClient  objstore.Bucket
	schemaConfig  chunk.SchemaConfig
	storageConfig storage.Config

	fetchedChunks     prometheus.Counter
	fetchedChunksSize prometheus.Counter
	processedSeries   prometheus.Counter
	writtenSamples    prometheus.Counter
	blocksSize        prometheus.Counter

	buildInProgress prometheus.Gauge
	chunksNotFound  prometheus.Counter
	seriesInMemory  prometheus.Gauge
}

func (b *Builder) cleanupFn(log log.Logger) error {
	files, err := ioutil.ReadDir(b.cfg.OutputDirectory)
	if err != nil {
		return err
	}

	// Delete directories with .tmp suffix (unfinished blocks).
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".tmp") && f.IsDir() {
			toRemove := filepath.Join(b.cfg.OutputDirectory, f.Name())

			level.Info(log).Log("msg", "deleting unfinished block", "dir", toRemove)

			err := os.RemoveAll(toRemove)
			if err != nil {
				return errors.Wrapf(err, "removing %s", toRemove)
			}
		}
	}

	return nil
}

func (b *Builder) planProcessorFactory(planLog log.Logger, userID string, start time.Time, end time.Time) planprocessor.PlanProcessor {
	return &builderProcessor{
		builder:  b,
		log:      planLog,
		userID:   userID,
		dayStart: start,
		dayEnd:   end,
	}
}

type builderProcessor struct {
	builder *Builder

	log      log.Logger
	userID   string
	dayStart time.Time
	dayEnd   time.Time
}

func (p *builderProcessor) ProcessPlanEntries(ctx context.Context, planEntryCh chan blocksconvert.PlanEntry) (string, error) {
	p.builder.buildInProgress.Set(1)
	defer p.builder.buildInProgress.Set(0)
	defer p.builder.seriesInMemory.Set(0)

	chunkClient, err := p.builder.createChunkClientForDay(p.dayStart)
	if err != nil {
		return "", errors.Wrap(err, "failed to create chunk client")
	}
	defer chunkClient.Stop()

	fetcher, err := newFetcher(p.userID, chunkClient, p.builder.fetchedChunks, p.builder.fetchedChunksSize)
	if err != nil {
		return "", errors.Wrap(err, "failed to create chunk fetcher")
	}

	tsdbBuilder, err := newTsdbBuilder(p.builder.cfg.OutputDirectory, p.dayStart, p.dayEnd, p.builder.cfg.TimestampTolerance, p.builder.cfg.SeriesBatchSize, p.log,
		p.builder.processedSeries, p.builder.writtenSamples, p.builder.seriesInMemory)
	if err != nil {
		return "", errors.Wrap(err, "failed to create TSDB builder")
	}

	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < p.builder.cfg.Concurrency; i++ {
		g.Go(func() error {
			return fetchAndBuild(gctx, fetcher, planEntryCh, tsdbBuilder, p.log, p.builder.chunksNotFound)
		})
	}

	if err := g.Wait(); err != nil {
		return "", errors.Wrap(err, "failed to build block")
	}

	// Finish block.
	ulid, err := tsdbBuilder.finishBlock("blocksconvert", map[string]string{
		mimir_tsdb.TenantIDExternalLabel: p.userID,
	})
	if err != nil {
		return "", errors.Wrap(err, "failed to finish block building")
	}

	blockDir := filepath.Join(p.builder.cfg.OutputDirectory, ulid.String())
	blockSize, err := getBlockSize(blockDir)
	if err != nil {
		return "", errors.Wrap(err, "block size")
	}

	level.Info(p.log).Log("msg", "successfully built block for a plan", "ulid", ulid.String(), "size", blockSize)
	p.builder.blocksSize.Add(float64(blockSize))

	if p.builder.cfg.UploadBlock {
		// No per-tenant config provider because the blocksconvert tool doesn't support it.
		userBucket := bucket.NewUserBucketClient(p.userID, p.builder.bucketClient, nil)

		err := uploadBlock(ctx, p.log, userBucket, blockDir)
		if err != nil {
			return "", errors.Wrap(err, "uploading block")
		}

		level.Info(p.log).Log("msg", "block uploaded", "ulid", ulid.String())

		if p.builder.cfg.DeleteLocalBlock {
			if err := os.RemoveAll(blockDir); err != nil {
				level.Warn(p.log).Log("msg", "failed to delete local block", "err", err)
			}
		}
	}

	// All OK
	return ulid.String(), nil
}

func uploadBlock(ctx context.Context, planLog log.Logger, userBucket objstore.Bucket, blockDir string) error {
	boff := backoff.New(ctx, backoff.Config{
		MinBackoff: 1 * time.Second,
		MaxBackoff: 5 * time.Second,
		MaxRetries: 5,
	})

	for boff.Ongoing() {
		err := block.Upload(ctx, planLog, userBucket, blockDir, metadata.NoneFunc)
		if err == nil {
			return nil
		}

		level.Warn(planLog).Log("msg", "failed to upload block", "err", err)
		boff.Wait()
	}

	return boff.Err()
}

func getBlockSize(dir string) (int64, error) {
	size := int64(0)

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			size += info.Size()
		}

		// Ignore directory with temporary series files.
		if info.IsDir() && info.Name() == "series" {
			return filepath.SkipDir
		}

		return nil
	})
	return size, err
}

func fetchAndBuild(ctx context.Context, f *Fetcher, input chan blocksconvert.PlanEntry, tb *tsdbBuilder, log log.Logger, chunksNotFound prometheus.Counter) error {
	b := backoff.New(ctx, backoff.Config{
		MinBackoff: 1 * time.Second,
		MaxBackoff: 5 * time.Second,
		MaxRetries: 5,
	})

	for {
		select {
		case <-ctx.Done():
			return nil

		case e, ok := <-input:
			if !ok {
				// End of input.
				return nil
			}

			var m labels.Labels
			var cs []chunk.Chunk
			var err error

			// Rather than aborting entire block build due to temporary errors ("connection reset by peer", "http2: client conn not usable"),
			// try to fetch chunks multiple times.
			for b.Reset(); b.Ongoing(); {
				m, cs, err = fetchAndBuildSingleSeries(ctx, f, e.Chunks)
				if err == nil {
					break
				}

				if b.Ongoing() {
					level.Warn(log).Log("msg", "failed to fetch chunks for series", "series", e.SeriesID, "err", err, "retries", b.NumRetries()+1)
					b.Wait()
				}
			}

			if err == nil {
				err = b.Err()
			}
			if err != nil {
				return errors.Wrapf(err, "failed to fetch chunks for series %s", e.SeriesID)
			}

			if len(e.Chunks) > len(cs) {
				chunksNotFound.Add(float64(len(e.Chunks) - len(cs)))
				level.Warn(log).Log("msg", "chunks for series not found", "seriesID", e.SeriesID, "expected", len(e.Chunks), "got", len(cs))
			}

			if len(cs) == 0 {
				continue
			}

			err = tb.buildSingleSeries(m, cs)
			if err != nil {
				return errors.Wrapf(err, "failed to build series %s", e.SeriesID)
			}
		}
	}
}

func fetchAndBuildSingleSeries(ctx context.Context, fetcher *Fetcher, chunksIds []string) (labels.Labels, []chunk.Chunk, error) {
	cs, err := fetcher.fetchChunks(ctx, chunksIds)
	if err != nil && !errors.Is(err, chunk.ErrStorageObjectNotFound) {
		return nil, nil, errors.Wrap(err, "fetching chunks")
	}

	if len(cs) == 0 {
		return nil, nil, nil
	}

	m, err := normalizeLabels(cs[0].Metric)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "chunk has invalid metrics: %v", cs[0].Metric.String())
	}

	// Verify that all chunks belong to the same series.
	for _, c := range cs {
		nm, err := normalizeLabels(c.Metric)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "chunk has invalid metrics: %v", c.Metric.String())
		}
		if !labels.Equal(m, nm) {
			return nil, nil, errors.Errorf("chunks for multiple metrics: %v, %v", m.String(), c.Metric.String())
		}
	}

	return m, cs, nil
}

// Labels are already sorted, but there may be duplicate label names.
// This method verifies sortedness, and removes duplicate label names (if they have the same value).
func normalizeLabels(lbls labels.Labels) (labels.Labels, error) {
	err := checkLabels(lbls)
	if err == errLabelsNotSorted {
		sort.Sort(lbls)
		err = checkLabels(lbls)
	}

	if err == errDuplicateLabelsSameValue {
		lbls = removeDuplicateLabels(lbls)
		err = checkLabels(lbls)
	}

	return lbls, err
}

var (
	errLabelsNotSorted               = errors.New("labels not sorted")
	errDuplicateLabelsSameValue      = errors.New("duplicate labels, same value")
	errDuplicateLabelsDifferentValue = errors.New("duplicate labels, different values")
)

// Returns one of errLabelsNotSorted, errDuplicateLabelsSameValue, errDuplicateLabelsDifferentValue,
// or nil, if labels are fine.
func checkLabels(lbls labels.Labels) error {
	prevName, prevValue := "", ""

	uniqueLabels := true
	for _, l := range lbls {
		switch {
		case l.Name < prevName:
			return errLabelsNotSorted
		case l.Name == prevName:
			if l.Value != prevValue {
				return errDuplicateLabelsDifferentValue
			}

			uniqueLabels = false
		}

		prevName = l.Name
		prevValue = l.Value
	}

	if !uniqueLabels {
		return errDuplicateLabelsSameValue
	}

	return nil
}

func removeDuplicateLabels(lbls labels.Labels) labels.Labels {
	prevName, prevValue := "", ""

	for ix := 0; ix < len(lbls); {
		l := lbls[ix]
		if l.Name == prevName && l.Value == prevValue {
			lbls = append(lbls[:ix], lbls[ix+1:]...)
			continue
		}

		prevName = l.Name
		prevValue = l.Value
		ix++
	}

	return lbls
}

// Finds storage configuration for given day, and builds a client.
func (b *Builder) createChunkClientForDay(dayStart time.Time) (chunk.Client, error) {
	for ix, s := range b.schemaConfig.Configs {
		if dayStart.Unix() < s.From.Unix() {
			continue
		}

		if ix+1 < len(b.schemaConfig.Configs) && dayStart.Unix() > b.schemaConfig.Configs[ix+1].From.Unix() {
			continue
		}

		objectStoreType := s.ObjectType
		if objectStoreType == "" {
			objectStoreType = s.IndexType
		}
		// No registerer, to avoid problems with registering same metrics multiple times.
		chunks, err := storage.NewChunkClient(objectStoreType, b.storageConfig, b.schemaConfig, nil)
		if err != nil {
			return nil, errors.Wrap(err, "error creating object client")
		}
		return chunks, nil
	}

	return nil, errors.Errorf("no schema for day %v", dayStart.Format("2006-01-02"))
}
