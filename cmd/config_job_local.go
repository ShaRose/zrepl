package cmd

import (
	"time"

	"context"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
	"github.com/zrepl/zrepl/cmd/endpoint"
	"github.com/zrepl/zrepl/replication"
	"github.com/zrepl/zrepl/zfs"
	"sync"
)

type LocalJob struct {
	Name           string
	Mapping        *DatasetMapFilter
	SnapshotPrefix string
	Interval       time.Duration
	PruneLHS       PrunePolicy
	PruneRHS       PrunePolicy
	Debug          JobDebugSettings
}

func parseLocalJob(c JobParsingContext, name string, i map[string]interface{}) (j *LocalJob, err error) {

	var asMap struct {
		Mapping           map[string]string
		SnapshotPrefix    string `mapstructure:"snapshot_prefix"`
		Interval          string
		InitialReplPolicy string                 `mapstructure:"initial_repl_policy"`
		PruneLHS          map[string]interface{} `mapstructure:"prune_lhs"`
		PruneRHS          map[string]interface{} `mapstructure:"prune_rhs"`
		Debug             map[string]interface{}
	}

	if err = mapstructure.Decode(i, &asMap); err != nil {
		err = errors.Wrap(err, "mapstructure error")
		return nil, err
	}

	j = &LocalJob{Name: name}

	if j.Mapping, err = parseDatasetMapFilter(asMap.Mapping, false); err != nil {
		return
	}

	if j.SnapshotPrefix, err = parseSnapshotPrefix(asMap.SnapshotPrefix); err != nil {
		return
	}

	if j.Interval, err = parsePostitiveDuration(asMap.Interval); err != nil {
		err = errors.Wrap(err, "cannot parse interval")
		return
	}

	if j.PruneLHS, err = parsePrunePolicy(asMap.PruneLHS, true); err != nil {
		err = errors.Wrap(err, "cannot parse 'prune_lhs'")
		return
	}
	if j.PruneRHS, err = parsePrunePolicy(asMap.PruneRHS, false); err != nil {
		err = errors.Wrap(err, "cannot parse 'prune_rhs'")
		return
	}

	if err = mapstructure.Decode(asMap.Debug, &j.Debug); err != nil {
		err = errors.Wrap(err, "cannot parse 'debug'")
		return
	}

	return
}

func (j *LocalJob) JobName() string {
	return j.Name
}

func (j *LocalJob) JobType() JobType { return JobTypeLocal }

func (j *LocalJob) JobStart(ctx context.Context) {

	log := getLogger(ctx)

	// Allow access to any dataset since we control what mapping
	// is passed to the pull routine.
	// All local datasets will be passed to its Map() function,
	// but only those for which a mapping exists will actually be pulled.
	// We can pay this small performance penalty for now.
	wildcardMapFilter := NewDatasetMapFilter(1, false)
	wildcardMapFilter.Add("<", "<")
	sender := endpoint.NewSender(wildcardMapFilter, NewPrefixFilter(j.SnapshotPrefix))

	receiver, err := endpoint.NewReceiver(j.Mapping, NewPrefixFilter(j.SnapshotPrefix))
	if err != nil {
		log.WithError(err).Error("unexpected error setting up local handler")
	}

	snapper := IntervalAutosnap{
		DatasetFilter:    j.Mapping.AsFilter(),
		Prefix:           j.SnapshotPrefix,
		SnapshotInterval: j.Interval,
	}

	plhs, err := j.Pruner(PrunePolicySideLeft, false)
	if err != nil {
		log.WithError(err).Error("error creating lhs pruner")
		return
	}
	prhs, err := j.Pruner(PrunePolicySideRight, false)
	if err != nil {
		log.WithError(err).Error("error creating rhs pruner")
		return
	}

	didSnaps := make(chan struct{})
	go snapper.Run(WithLogger(ctx, log.WithField(logSubsysField, "snap")), didSnaps)

outer:
	for {

		select {
		case <-ctx.Done():
			log.WithError(ctx.Err()).Info("context")
			break outer
		case <-didSnaps:
			log.Debug("finished taking snapshots")
			log.Info("starting replication procedure")
		}

		{
			ctx := WithLogger(ctx, log.WithField(logSubsysField, "replication"))
			rep := replication.NewReplication()
			rep.Drive(ctx, sender, receiver)
		}

		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			plhs.Run(WithLogger(ctx, log.WithField(logSubsysField, "prune_lhs")))
			wg.Done()
		}()

		wg.Add(1)
		go func() {
			prhs.Run(WithLogger(ctx, log.WithField(logSubsysField, "prune_rhs")))
			wg.Done()
		}()

		wg.Wait()
	}

}

func (j *LocalJob) Pruner(side PrunePolicySide, dryRun bool) (p Pruner, err error) {

	var dsfilter zfs.DatasetFilter
	var pp PrunePolicy
	switch side {
	case PrunePolicySideLeft:
		pp = j.PruneLHS
		dsfilter = j.Mapping.AsFilter()
	case PrunePolicySideRight:
		pp = j.PruneRHS
		dsfilter, err = j.Mapping.InvertedFilter()
		if err != nil {
			err = errors.Wrap(err, "cannot invert mapping for prune_rhs")
			return
		}
	default:
		err = errors.Errorf("must be either left or right side")
		return
	}

	p = Pruner{
		time.Now(),
		dryRun,
		dsfilter,
		j.SnapshotPrefix,
		pp,
	}

	return
}
