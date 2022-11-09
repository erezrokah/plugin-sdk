package plugins

import (
	"context"
	"time"

	"github.com/cloudquery/plugin-sdk/schema"
	"github.com/cloudquery/plugin-sdk/specs"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

type NewDestinationClientFunc func(context.Context, zerolog.Logger, specs.Destination) (DestinationClient, error)

type DestinationClient interface {
	schema.CQTypeTransformer
	ReverseTransformValues(table *schema.Table, values []interface{}) (schema.CQTypes, error)
	Migrate(ctx context.Context, tables schema.Tables) error
	Read(ctx context.Context, table *schema.Table, sourceName string, res chan<- []interface{}) error
	Write(ctx context.Context, tables schema.Tables, res <-chan *ClientResource) error
	Metrics() DestinationMetrics
	DeleteStale(ctx context.Context, tables schema.Tables, sourceName string, syncTime time.Time) error
	Close(ctx context.Context) error
}

type ClientResource struct {
	TableName string
	Data      []interface{}
}

type DestinationPlugin struct {
	// Name of destination plugin i.e postgresql,snowflake
	name string
	// Version of the destination plugin
	version string
	// Called upon configure call to validate and init configuration
	newDestinationClient NewDestinationClientFunc
	// initialized destination client
	client DestinationClient
	// spec the client was initialized with
	spec specs.Destination
	// Logger to call, this logger is passed to the serve.Serve Client, if not define Serve will create one instead.
	logger zerolog.Logger
}

const writeWorkers = 1

func NewDestinationPlugin(name string, version string, newDestinationClient NewDestinationClientFunc) *DestinationPlugin {
	p := &DestinationPlugin{
		name:                 name,
		version:              version,
		newDestinationClient: newDestinationClient,
	}
	return p
}

func (p *DestinationPlugin) Name() string {
	return p.name
}

func (p *DestinationPlugin) Version() string {
	return p.version
}

func (p *DestinationPlugin) Metrics() DestinationMetrics {
	return p.client.Metrics()
}

// we need lazy loading because we want to be able to initialize after
func (p *DestinationPlugin) Init(ctx context.Context, logger zerolog.Logger, spec specs.Destination) error {
	var err error
	p.logger = logger
	p.spec = spec
	p.client, err = p.newDestinationClient(ctx, logger, spec)
	if err != nil {
		return err
	}
	return nil
}

// we implement all DestinationClient functions so we can hook into pre-post behavior
func (p *DestinationPlugin) Migrate(ctx context.Context, tables schema.Tables) error {
	SetDestinationManagedCqColumns(tables)
	return p.client.Migrate(ctx, tables)
}

func (p *DestinationPlugin) readAll(ctx context.Context, table *schema.Table, sourceName string) ([]schema.CQTypes, error) {
	var readErr error
	ch := make(chan schema.CQTypes)
	go func() {
		defer close(ch)
		readErr = p.Read(ctx, table, sourceName, ch)
	}()
	//nolint:prealloc
	var resources []schema.CQTypes
	for resource := range ch {
		resources = append(resources, resource)
	}
	return resources, readErr
}

func (p *DestinationPlugin) Read(ctx context.Context, table *schema.Table, sourceName string, res chan<- schema.CQTypes) error {
	SetDestinationManagedCqColumns(schema.Tables{table})
	ch := make(chan []interface{})
	var err error
	go func() {
		defer close(ch)
		err = p.client.Read(ctx, table, sourceName, ch)
	}()
	for resource := range ch {
		r, err := p.client.ReverseTransformValues(table, resource)
		if err != nil {
			return err
		}
		res <- r
	}
	return err
}

// this function is currently used mostly for testing so it's not a public api
func (p *DestinationPlugin) writeOne(ctx context.Context, tables schema.Tables, sourceName string, syncTime time.Time, resource schema.DestinationResource) error {
	resources := []schema.DestinationResource{resource}
	return p.writeAll(ctx, tables, sourceName, syncTime, resources)
}

// this function is currently used mostly for testing so it's not a public api
func (p *DestinationPlugin) writeAll(ctx context.Context, tables schema.Tables, sourceName string, syncTime time.Time, resources []schema.DestinationResource) error {
	ch := make(chan schema.DestinationResource, len(resources))
	for _, resource := range resources {
		ch <- resource
	}
	close(ch)
	return p.Write(ctx, tables, sourceName, syncTime, ch)
}

func (p *DestinationPlugin) Write(ctx context.Context, tables schema.Tables, sourceName string, syncTime time.Time, res <-chan schema.DestinationResource) error {
	syncTime = syncTime.UTC()
	SetDestinationManagedCqColumns(tables)
	ch := make(chan *ClientResource)
	eg, ctx := errgroup.WithContext(ctx)
	// given most destination plugins writing in batch we are using a worker pool to write in parallel
	// it might not generalize well and we might need to move it to each destination plugin implementation.
	for i := 0; i < writeWorkers; i++ {
		eg.Go(func() error {
			return p.client.Write(ctx, tables, ch)
		})
	}
	sourceColumn := &schema.Text{}
	_ = sourceColumn.Set(sourceName)
	syncTimeColumn := &schema.Timestamptz{}
	_ = syncTimeColumn.Set(syncTime)
	for r := range res {
		r.Data = append([]schema.CQType{sourceColumn, syncTimeColumn}, r.Data...)
		clientResource := &ClientResource{
			TableName: r.TableName,
			Data:      schema.TransformWithTransformer(p.client, r.Data),
		}
		select {
		case <-ctx.Done():
			close(ch)
			return eg.Wait()
		case ch <- clientResource:
		}
	}

	close(ch)
	if err := eg.Wait(); err != nil {
		return err
	}
	if p.spec.WriteMode == specs.WriteModeOverwriteDeleteStale {
		if err := p.DeleteStale(ctx, tables, sourceName, syncTime); err != nil {
			return err
		}
	}
	return nil
}

func (p *DestinationPlugin) DeleteStale(ctx context.Context, tables schema.Tables, sourceName string, syncTime time.Time) error {
	syncTime = syncTime.UTC()
	return p.client.DeleteStale(ctx, tables, sourceName, syncTime)
}

func (p *DestinationPlugin) Close(ctx context.Context) error {
	return p.client.Close(ctx)
}

// Overwrites or adds the CQ columns that are managed by the destination plugins (_cq_sync_time, _cq_source_name).
func SetDestinationManagedCqColumns(tables []*schema.Table) {
	for _, table := range tables {
		table.OverwriteOrAddColumn(&schema.CqSyncTimeColumn)
		table.OverwriteOrAddColumn(&schema.CqSourceNameColumn)

		SetDestinationManagedCqColumns(table.Relations)
	}
}
