// Copyright (C) 2022 NHR@FAU, University Erlangen-Nuremberg.
// All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.
package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/ClusterCockpit/cc-backend/internal/auth"
	"github.com/ClusterCockpit/cc-backend/internal/config"
	"github.com/ClusterCockpit/cc-backend/internal/graph/model"
	"github.com/ClusterCockpit/cc-backend/internal/metricdata"
	"github.com/ClusterCockpit/cc-backend/pkg/archive"
	"github.com/ClusterCockpit/cc-backend/pkg/log"
	"github.com/ClusterCockpit/cc-backend/pkg/lrucache"
	"github.com/ClusterCockpit/cc-backend/pkg/schema"
	sq "github.com/Masterminds/squirrel"
	"github.com/jmoiron/sqlx"
)

var (
	jobRepoOnce     sync.Once
	jobRepoInstance *JobRepository
)

type JobRepository struct {
	DB     *sqlx.DB
	driver string

	stmtCache *sq.StmtCache
	cache     *lrucache.Cache

	archiveChannel chan *schema.Job
	archivePending sync.WaitGroup
}

func GetJobRepository() *JobRepository {
	jobRepoOnce.Do(func() {
		db := GetConnection()

		jobRepoInstance = &JobRepository{
			DB:     db.DB,
			driver: db.Driver,

			stmtCache:      sq.NewStmtCache(db.DB),
			cache:          lrucache.New(1024 * 1024),
			archiveChannel: make(chan *schema.Job, 128),
		}
		// start archiving worker
		go jobRepoInstance.archivingWorker()
	})

	return jobRepoInstance
}

var jobColumns []string = []string{
	"job.id", "job.job_id", "job.user", "job.project", "job.cluster", "job.subcluster", "job.start_time", "job.partition", "job.array_job_id",
	"job.num_nodes", "job.num_hwthreads", "job.num_acc", "job.exclusive", "job.monitoring_status", "job.smt", "job.job_state",
	"job.duration", "job.walltime", "job.resources", // "job.meta_data",
}

func scanJob(row interface{ Scan(...interface{}) error }) (*schema.Job, error) {
	job := &schema.Job{}
	if err := row.Scan(
		&job.ID, &job.JobID, &job.User, &job.Project, &job.Cluster, &job.SubCluster, &job.StartTimeUnix, &job.Partition, &job.ArrayJobId,
		&job.NumNodes, &job.NumHWThreads, &job.NumAcc, &job.Exclusive, &job.MonitoringStatus, &job.SMT, &job.State,
		&job.Duration, &job.Walltime, &job.RawResources /*&job.RawMetaData*/); err != nil {
		log.Warn("Error while scanning rows")
		return nil, err
	}

	if err := json.Unmarshal(job.RawResources, &job.Resources); err != nil {
		log.Warn("Error while unmarhsaling raw resources json")
		return nil, err
	}

	// if err := json.Unmarshal(job.RawMetaData, &job.MetaData); err != nil {
	// 	return nil, err
	// }

	job.StartTime = time.Unix(job.StartTimeUnix, 0)
	if job.Duration == 0 && job.State == schema.JobStateRunning {
		job.Duration = int32(time.Since(job.StartTime).Seconds())
	}

	job.RawResources = nil
	return job, nil
}

func (r *JobRepository) FetchJobName(job *schema.Job) (*string, error) {
	start := time.Now()
	cachekey := fmt.Sprintf("metadata:%d", job.ID)
	if cached := r.cache.Get(cachekey, nil); cached != nil {
		job.MetaData = cached.(map[string]string)
		if jobName := job.MetaData["jobName"]; jobName != "" {
			return &jobName, nil
		}
	}

	if err := sq.Select("job.meta_data").From("job").Where("job.id = ?", job.ID).
		RunWith(r.stmtCache).QueryRow().Scan(&job.RawMetaData); err != nil {
		return nil, err
	}

	if len(job.RawMetaData) == 0 {
		return nil, nil
	}

	if err := json.Unmarshal(job.RawMetaData, &job.MetaData); err != nil {
		return nil, err
	}

	r.cache.Put(cachekey, job.MetaData, len(job.RawMetaData), 24*time.Hour)
	log.Infof("Timer FetchJobName %s", time.Since(start))

	if jobName := job.MetaData["jobName"]; jobName != "" {
		return &jobName, nil
	} else {
		return new(string), nil
	}
}

func (r *JobRepository) FetchMetadata(job *schema.Job) (map[string]string, error) {
	start := time.Now()
	cachekey := fmt.Sprintf("metadata:%d", job.ID)
	if cached := r.cache.Get(cachekey, nil); cached != nil {
		job.MetaData = cached.(map[string]string)
		return job.MetaData, nil
	}

	if err := sq.Select("job.meta_data").From("job").Where("job.id = ?", job.ID).
		RunWith(r.stmtCache).QueryRow().Scan(&job.RawMetaData); err != nil {
		log.Warn("Error while scanning for job metadata")
		return nil, err
	}

	if len(job.RawMetaData) == 0 {
		return nil, nil
	}

	if err := json.Unmarshal(job.RawMetaData, &job.MetaData); err != nil {
		log.Warn("Error while unmarshaling raw metadata json")
		return nil, err
	}

	r.cache.Put(cachekey, job.MetaData, len(job.RawMetaData), 24*time.Hour)
	log.Infof("Timer FetchMetadata %s", time.Since(start))
	return job.MetaData, nil
}

func (r *JobRepository) UpdateMetadata(job *schema.Job, key, val string) (err error) {
	cachekey := fmt.Sprintf("metadata:%d", job.ID)
	r.cache.Del(cachekey)
	if job.MetaData == nil {
		if _, err = r.FetchMetadata(job); err != nil {
			log.Warnf("Error while fetching metadata for job, DB ID '%v'", job.ID)
			return err
		}
	}

	if job.MetaData != nil {
		cpy := make(map[string]string, len(job.MetaData)+1)
		for k, v := range job.MetaData {
			cpy[k] = v
		}
		cpy[key] = val
		job.MetaData = cpy
	} else {
		job.MetaData = map[string]string{key: val}
	}

	if job.RawMetaData, err = json.Marshal(job.MetaData); err != nil {
		log.Warnf("Error while marshaling metadata for job, DB ID '%v'", job.ID)
		return err
	}

	if _, err = sq.Update("job").Set("meta_data", job.RawMetaData).Where("job.id = ?", job.ID).RunWith(r.stmtCache).Exec(); err != nil {
		log.Warnf("Error while updating metadata for job, DB ID '%v'", job.ID)
		return err
	}

	r.cache.Put(cachekey, job.MetaData, len(job.RawMetaData), 24*time.Hour)
	return nil
}

// Find executes a SQL query to find a specific batch job.
// The job is queried using the batch job id, the cluster name,
// and the start time of the job in UNIX epoch time seconds.
// It returns a pointer to a schema.Job data structure and an error variable.
// To check if no job was found test err == sql.ErrNoRows
func (r *JobRepository) Find(
	jobId *int64,
	cluster *string,
	startTime *int64) (*schema.Job, error) {

	start := time.Now()
	q := sq.Select(jobColumns...).From("job").
		Where("job.job_id = ?", *jobId)

	if cluster != nil {
		q = q.Where("job.cluster = ?", *cluster)
	}
	if startTime != nil {
		q = q.Where("job.start_time = ?", *startTime)
	}

	log.Infof("Timer Find %s", time.Since(start))
	return scanJob(q.RunWith(r.stmtCache).QueryRow())
}

// Find executes a SQL query to find a specific batch job.
// The job is queried using the batch job id, the cluster name,
// and the start time of the job in UNIX epoch time seconds.
// It returns a pointer to a schema.Job data structure and an error variable.
// To check if no job was found test err == sql.ErrNoRows
func (r *JobRepository) FindAll(
	jobId *int64,
	cluster *string,
	startTime *int64) ([]*schema.Job, error) {

	start := time.Now()
	q := sq.Select(jobColumns...).From("job").
		Where("job.job_id = ?", *jobId)

	if cluster != nil {
		q = q.Where("job.cluster = ?", *cluster)
	}
	if startTime != nil {
		q = q.Where("job.start_time = ?", *startTime)
	}

	rows, err := q.RunWith(r.stmtCache).Query()
	if err != nil {
		log.Error("Error while running query")
		return nil, err
	}

	jobs := make([]*schema.Job, 0, 10)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			log.Warn("Error while scanning rows")
			return nil, err
		}
		jobs = append(jobs, job)
	}
	log.Infof("Timer FindAll %s", time.Since(start))
	return jobs, nil
}

// FindById executes a SQL query to find a specific batch job.
// The job is queried using the database id.
// It returns a pointer to a schema.Job data structure and an error variable.
// To check if no job was found test err == sql.ErrNoRows
func (r *JobRepository) FindById(jobId int64) (*schema.Job, error) {
	q := sq.Select(jobColumns...).
		From("job").Where("job.id = ?", jobId)
	return scanJob(q.RunWith(r.stmtCache).QueryRow())
}

// Start inserts a new job in the table, returning the unique job ID.
// Statistics are not transfered!
func (r *JobRepository) Start(job *schema.JobMeta) (id int64, err error) {
	job.RawResources, err = json.Marshal(job.Resources)
	if err != nil {
		return -1, fmt.Errorf("REPOSITORY/JOB > encoding resources field failed: %w", err)
	}

	job.RawMetaData, err = json.Marshal(job.MetaData)
	if err != nil {
		return -1, fmt.Errorf("REPOSITORY/JOB > encoding metaData field failed: %w", err)
	}

	res, err := r.DB.NamedExec(`INSERT INTO job (
		job_id, user, project, cluster, subcluster, `+"`partition`"+`, array_job_id, num_nodes, num_hwthreads, num_acc,
		exclusive, monitoring_status, smt, job_state, start_time, duration, walltime, resources, meta_data
	) VALUES (
		:job_id, :user, :project, :cluster, :subcluster, :partition, :array_job_id, :num_nodes, :num_hwthreads, :num_acc,
		:exclusive, :monitoring_status, :smt, :job_state, :start_time, :duration, :walltime, :resources, :meta_data
	);`, job)
	if err != nil {
		return -1, err
	}

	return res.LastInsertId()
}

// Stop updates the job with the database id jobId using the provided arguments.
func (r *JobRepository) Stop(
	jobId int64,
	duration int32,
	state schema.JobState,
	monitoringStatus int32) (err error) {

	stmt := sq.Update("job").
		Set("job_state", state).
		Set("duration", duration).
		Set("monitoring_status", monitoringStatus).
		Where("job.id = ?", jobId)

	_, err = stmt.RunWith(r.stmtCache).Exec()
	return
}

func (r *JobRepository) DeleteJobsBefore(startTime int64) (int, error) {
	var cnt int
	qs := fmt.Sprintf("SELECT count(*) FROM job WHERE job.start_time < %d", startTime)
	err := r.DB.Get(&cnt, qs) //ignore error as it will also occur in delete statement
	_, err = r.DB.Exec(`DELETE FROM job WHERE job.start_time < ?`, startTime)
	if err != nil {
		log.Errorf(" DeleteJobsBefore(%d): error %#v", startTime, err)
	} else {
		log.Infof("DeleteJobsBefore(%d): Deleted %d jobs", startTime, cnt)
	}
	return cnt, err
}

func (r *JobRepository) DeleteJobById(id int64) error {
	_, err := r.DB.Exec(`DELETE FROM job WHERE job.id = ?`, id)
	if err != nil {
		log.Errorf("DeleteJobById(%d): error %#v", id, err)
	} else {
		log.Infof("DeleteJobById(%d): Success", id)
	}
	return err
}

// TODO: Use node hours instead: SELECT job.user, sum(job.num_nodes * (CASE WHEN job.job_state = "running" THEN CAST(strftime('%s', 'now') AS INTEGER) - job.start_time ELSE job.duration END)) as x FROM job GROUP BY user ORDER BY x DESC;
func (r *JobRepository) CountGroupedJobs(
	ctx context.Context,
	aggreg model.Aggregate,
	filters []*model.JobFilter,
	weight *model.Weights,
	limit *int) (map[string]int, error) {

	start := time.Now()
	if !aggreg.IsValid() {
		return nil, errors.New("invalid aggregate")
	}

	runner := (sq.BaseRunner)(r.stmtCache)
	count := "count(*) as count"
	if weight != nil {
		switch *weight {
		case model.WeightsNodeCount:
			count = "sum(job.num_nodes) as count"
		case model.WeightsNodeHours:
			now := time.Now().Unix()
			count = fmt.Sprintf(`sum(job.num_nodes * (CASE WHEN job.job_state = "running" THEN %d - job.start_time ELSE job.duration END)) as count`, now)
			runner = r.DB
		default:
			log.Infof("CountGroupedJobs() Weight %v unknown.", *weight)
		}
	}

	q, qerr := SecurityCheck(ctx, sq.Select("job."+string(aggreg), count).From("job").GroupBy("job."+string(aggreg)).OrderBy("count DESC"))

	if qerr != nil {
		return nil, qerr
	}

	for _, f := range filters {
		q = BuildWhereClause(f, q)
	}
	if limit != nil {
		q = q.Limit(uint64(*limit))
	}

	counts := map[string]int{}
	rows, err := q.RunWith(runner).Query()
	if err != nil {
		log.Error("Error while running query")
		return nil, err
	}

	for rows.Next() {
		var group string
		var count int
		if err := rows.Scan(&group, &count); err != nil {
			log.Warn("Error while scanning rows")
			return nil, err
		}

		counts[group] = count
	}

	log.Infof("Timer CountGroupedJobs %s", time.Since(start))
	return counts, nil
}

func (r *JobRepository) UpdateMonitoringStatus(job int64, monitoringStatus int32) (err error) {
	stmt := sq.Update("job").
		Set("monitoring_status", monitoringStatus).
		Where("job.id = ?", job)

	_, err = stmt.RunWith(r.stmtCache).Exec()
	return
}

// Stop updates the job with the database id jobId using the provided arguments.
func (r *JobRepository) MarkArchived(
	jobId int64,
	monitoringStatus int32,
	metricStats map[string]schema.JobStatistics) error {

	stmt := sq.Update("job").
		Set("monitoring_status", monitoringStatus).
		Where("job.id = ?", jobId)

	for metric, stats := range metricStats {
		switch metric {
		case "flops_any":
			stmt = stmt.Set("flops_any_avg", stats.Avg)
		case "mem_used":
			stmt = stmt.Set("mem_used_max", stats.Max)
		case "mem_bw":
			stmt = stmt.Set("mem_bw_avg", stats.Avg)
		case "load":
			stmt = stmt.Set("load_avg", stats.Avg)
		case "net_bw":
			stmt = stmt.Set("net_bw_avg", stats.Avg)
		case "file_bw":
			stmt = stmt.Set("file_bw_avg", stats.Avg)
		default:
			log.Infof("MarkArchived() Metric '%v' unknown", metric)
		}
	}

	if _, err := stmt.RunWith(r.stmtCache).Exec(); err != nil {
		log.Warn("Error while marking job as archived")
		return err
	}
	return nil
}

// Archiving worker thread
func (r *JobRepository) archivingWorker() {
	for {
		select {
		case job, ok := <-r.archiveChannel:
			if !ok {
				break
			}
			// not using meta data, called to load JobMeta into Cache?
			// will fail if job meta not in repository
			if _, err := r.FetchMetadata(job); err != nil {
				log.Errorf("archiving job (dbid: %d) failed: %s", job.ID, err.Error())
				r.UpdateMonitoringStatus(job.ID, schema.MonitoringStatusArchivingFailed)
				continue
			}

			// metricdata.ArchiveJob will fetch all the data from a MetricDataRepository and push into configured archive backend
			// TODO: Maybe use context with cancel/timeout here
			jobMeta, err := metricdata.ArchiveJob(job, context.Background())
			if err != nil {
				log.Errorf("archiving job (dbid: %d) failed: %s", job.ID, err.Error())
				r.UpdateMonitoringStatus(job.ID, schema.MonitoringStatusArchivingFailed)
				continue
			}

			// Update the jobs database entry one last time:
			if err := r.MarkArchived(job.ID, schema.MonitoringStatusArchivingSuccessful, jobMeta.Statistics); err != nil {
				log.Errorf("archiving job (dbid: %d) failed: %s", job.ID, err.Error())
				continue
			}

			log.Printf("archiving job (dbid: %d) successful", job.ID)
			r.archivePending.Done()
		}
	}
}

// Trigger async archiving
func (r *JobRepository) TriggerArchiving(job *schema.Job) {
	r.archivePending.Add(1)
	r.archiveChannel <- job
}

// Wait for background thread to finish pending archiving operations
func (r *JobRepository) WaitForArchiving() {
	// close channel and wait for worker to process remaining jobs
	r.archivePending.Wait()
}

var ErrNotFound = errors.New("no such jobname, project or user")
var ErrForbidden = errors.New("not authorized")

// FindJobnameOrUserOrProject returns a jobName or a username or a projectId if a jobName or user or project matches the search term.
// If query is found to be an integer (= conversion to INT datatype succeeds), skip back to parent call
// If nothing matches the search, `ErrNotFound` is returned.

func (r *JobRepository) FindUserOrProjectOrJobname(ctx context.Context, searchterm string) (username string, project string, metasnip string, err error) {
	if _, err := strconv.Atoi(searchterm); err == nil { // Return empty on successful conversion: parent method will redirect for integer jobId
		return "", "", "", nil
	} else { // Has to have letters and logged-in user for other guesses
		user := auth.GetUser(ctx)
		if user != nil {
			// Find username in jobs (match)
			uresult, _ := r.FindColumnValue(user, searchterm, "job", "user", "user", false)
			if uresult != "" {
				return uresult, "", "", nil
			}
			// Find username by name (like)
			nresult, _ := r.FindColumnValue(user, searchterm, "user", "username", "name", true)
			if nresult != "" {
				return nresult, "", "", nil
			}
			// Find projectId in jobs (match)
			presult, _ := r.FindColumnValue(user, searchterm, "job", "project", "project", false)
			if presult != "" {
				return "", presult, "", nil
			}
			// Still no return (or not authorized for above): Try JobName
			// Match Metadata, on hit, parent method redirects to jobName GQL query
			err := sq.Select("job.cluster").Distinct().From("job").
				Where("job.meta_data LIKE ?", "%"+searchterm+"%").
				RunWith(r.stmtCache).QueryRow().Scan(&metasnip)
			if err != nil && err != sql.ErrNoRows {
				return "", "", "", err
			} else if err == nil {
				return "", "", metasnip[0:1], nil
			}
		}
		return "", "", "", ErrNotFound
	}
}

func (r *JobRepository) FindColumnValue(user *auth.User, searchterm string, table string, selectColumn string, whereColumn string, isLike bool) (result string, err error) {
	compareStr := " = ?"
	query := searchterm
	if isLike == true {
		compareStr = " LIKE ?"
		query = "%" + searchterm + "%"
	}
	if user.HasAnyRole([]auth.Role{auth.RoleAdmin, auth.RoleSupport, auth.RoleManager}) {
		err := sq.Select(table+"."+selectColumn).Distinct().From(table).
			Where(table+"."+whereColumn+compareStr, query).
			RunWith(r.stmtCache).QueryRow().Scan(&result)
		if err != nil && err != sql.ErrNoRows {
			return "", err
		} else if err == nil {
			return result, nil
		}
		return "", ErrNotFound
	} else {
		log.Infof("Non-Admin User %s : Requested Query '%s' on table '%s' : Forbidden", user.Name, query, table)
		return "", ErrForbidden
	}
}

func (r *JobRepository) FindColumnValues(user *auth.User, query string, table string, selectColumn string, whereColumn string) (results []string, err error) {
	emptyResult := make([]string, 0)
	if user.HasAnyRole([]auth.Role{auth.RoleAdmin, auth.RoleSupport, auth.RoleManager}) {
		rows, err := sq.Select(table+"."+selectColumn).Distinct().From(table).
			Where(table+"."+whereColumn+" LIKE ?", fmt.Sprint("%", query, "%")).
			RunWith(r.stmtCache).Query()
		if err != nil && err != sql.ErrNoRows {
			return emptyResult, err
		} else if err == nil {
			for rows.Next() {
				var result string
				err := rows.Scan(&result)
				if err != nil {
					rows.Close()
					log.Warnf("Error while scanning rows: %v", err)
					return emptyResult, err
				}
				results = append(results, result)
			}
			return results, nil
		}
		return emptyResult, ErrNotFound

	} else {
		log.Infof("Non-Admin User %s : Requested Query '%s' on table '%s' : Forbidden", user.Name, query, table)
		return emptyResult, ErrForbidden
	}
}

func (r *JobRepository) Partitions(cluster string) ([]string, error) {
	var err error
	start := time.Now()
	partitions := r.cache.Get("partitions:"+cluster, func() (interface{}, time.Duration, int) {
		parts := []string{}
		if err = r.DB.Select(&parts, `SELECT DISTINCT job.partition FROM job WHERE job.cluster = ?;`, cluster); err != nil {
			return nil, 0, 1000
		}

		return parts, 1 * time.Hour, 1
	})
	if err != nil {
		return nil, err
	}
	log.Infof("Timer Partitions %s", time.Since(start))
	return partitions.([]string), nil
}

// AllocatedNodes returns a map of all subclusters to a map of hostnames to the amount of jobs running on that host.
// Hosts with zero jobs running on them will not show up!
func (r *JobRepository) AllocatedNodes(cluster string) (map[string]map[string]int, error) {

	start := time.Now()
	subclusters := make(map[string]map[string]int)
	rows, err := sq.Select("resources", "subcluster").From("job").
		Where("job.job_state = 'running'").
		Where("job.cluster = ?", cluster).
		RunWith(r.stmtCache).Query()
	if err != nil {
		log.Error("Error while running query")
		return nil, err
	}

	var raw []byte
	defer rows.Close()
	for rows.Next() {
		raw = raw[0:0]
		var resources []*schema.Resource
		var subcluster string
		if err := rows.Scan(&raw, &subcluster); err != nil {
			log.Warn("Error while scanning rows")
			return nil, err
		}
		if err := json.Unmarshal(raw, &resources); err != nil {
			log.Warn("Error while unmarshaling raw resources json")
			return nil, err
		}

		hosts, ok := subclusters[subcluster]
		if !ok {
			hosts = make(map[string]int)
			subclusters[subcluster] = hosts
		}

		for _, resource := range resources {
			hosts[resource.Hostname] += 1
		}
	}

	log.Infof("Timer AllocatedNodes %s", time.Since(start))
	return subclusters, nil
}

func (r *JobRepository) StopJobsExceedingWalltimeBy(seconds int) error {

	start := time.Now()
	res, err := sq.Update("job").
		Set("monitoring_status", schema.MonitoringStatusArchivingFailed).
		Set("duration", 0).
		Set("job_state", schema.JobStateFailed).
		Where("job.job_state = 'running'").
		Where("job.walltime > 0").
		Where(fmt.Sprintf("(%d - job.start_time) > (job.walltime + %d)", time.Now().Unix(), seconds)).
		RunWith(r.DB).Exec()
	if err != nil {
		log.Warn("Error while stopping jobs exceeding walltime")
		return err
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		log.Warn("Error while fetching affected rows after stopping due to exceeded walltime")
		return err
	}

	if rowsAffected > 0 {
		log.Infof("%d jobs have been marked as failed due to running too long", rowsAffected)
	}
	log.Infof("Timer StopJobsExceedingWalltimeBy %s", time.Since(start))
	return nil
}

// GraphQL validation should make sure that no unkown values can be specified.
var groupBy2column = map[model.Aggregate]string{
	model.AggregateUser:    "job.user",
	model.AggregateProject: "job.project",
	model.AggregateCluster: "job.cluster",
}

// Helper function for the jobsStatistics GraphQL query placed here so that schema.resolvers.go is not too full.
func (r *JobRepository) JobsStatistics(ctx context.Context,
	filter []*model.JobFilter,
	groupBy *model.Aggregate) ([]*model.JobsStatistics, error) {

	start := time.Now()
	// In case `groupBy` is nil (not used), the model.JobsStatistics used is at the key '' (empty string)
	stats := map[string]*model.JobsStatistics{}
	var castType string

	if r.driver == "sqlite3" {
		castType = "int"
	} else if r.driver == "mysql" {
		castType = "unsigned"
	}

	// `socketsPerNode` and `coresPerSocket` can differ from cluster to cluster, so we need to explicitly loop over those.
	for _, cluster := range archive.Clusters {
		for _, subcluster := range cluster.SubClusters {
			corehoursCol := fmt.Sprintf("CAST(ROUND(SUM(job.duration * job.num_nodes * %d * %d) / 3600) as %s)", subcluster.SocketsPerNode, subcluster.CoresPerSocket, castType)
			var rawQuery sq.SelectBuilder
			if groupBy == nil {
				rawQuery = sq.Select(
					"''",
					"COUNT(job.id)",
					fmt.Sprintf("CAST(ROUND(SUM(job.duration) / 3600) as %s)", castType),
					corehoursCol,
				).From("job")
			} else {
				col := groupBy2column[*groupBy]
				rawQuery = sq.Select(
					col,
					"COUNT(job.id)",
					fmt.Sprintf("CAST(ROUND(SUM(job.duration) / 3600) as %s)", castType),
					corehoursCol,
				).From("job").GroupBy(col)
			}

			rawQuery = rawQuery.
				Where("job.cluster = ?", cluster.Name).
				Where("job.subcluster = ?", subcluster.Name)

			query, qerr := SecurityCheck(ctx, rawQuery)

			if qerr != nil {
				return nil, qerr
			}

			for _, f := range filter {
				query = BuildWhereClause(f, query)
			}

			rows, err := query.RunWith(r.DB).Query()
			if err != nil {
				log.Warn("Error while querying DB for job statistics")
				return nil, err
			}

			for rows.Next() {
				var id sql.NullString
				var jobs, walltime, corehours sql.NullInt64
				if err := rows.Scan(&id, &jobs, &walltime, &corehours); err != nil {
					log.Warn("Error while scanning rows")
					return nil, err
				}

				if id.Valid {
					if s, ok := stats[id.String]; ok {
						s.TotalJobs += int(jobs.Int64)
						s.TotalWalltime += int(walltime.Int64)
						s.TotalCoreHours += int(corehours.Int64)
					} else {
						stats[id.String] = &model.JobsStatistics{
							ID:             id.String,
							TotalJobs:      int(jobs.Int64),
							TotalWalltime:  int(walltime.Int64),
							TotalCoreHours: int(corehours.Int64),
						}
					}
				}
			}

		}
	}

	if groupBy == nil {

		query := sq.Select("COUNT(job.id)").From("job").Where("job.duration < ?", config.Keys.ShortRunningJobsDuration)
		query, qerr := SecurityCheck(ctx, query)

		if qerr != nil {
			return nil, qerr
		}

		for _, f := range filter {
			query = BuildWhereClause(f, query)
		}
		if err := query.RunWith(r.DB).QueryRow().Scan(&(stats[""].ShortJobs)); err != nil {
			log.Warn("Error while scanning rows for short job stats")
			return nil, err
		}
	} else {
		col := groupBy2column[*groupBy]

		query := sq.Select(col, "COUNT(job.id)").From("job").Where("job.duration < ?", config.Keys.ShortRunningJobsDuration)

		query, qerr := SecurityCheck(ctx, query)

		if qerr != nil {
			return nil, qerr
		}

		for _, f := range filter {
			query = BuildWhereClause(f, query)
		}
		rows, err := query.RunWith(r.DB).Query()
		if err != nil {
			log.Warn("Error while querying jobs for short jobs")
			return nil, err
		}

		for rows.Next() {
			var id sql.NullString
			var shortJobs sql.NullInt64
			if err := rows.Scan(&id, &shortJobs); err != nil {
				log.Warn("Error while scanning rows for short jobs")
				return nil, err
			}

			if id.Valid {
				stats[id.String].ShortJobs = int(shortJobs.Int64)
			}
		}

		if col == "job.user" {
			for id := range stats {
				emptyDash := "-"
				user := auth.GetUser(ctx)
				name, _ := r.FindColumnValue(user, id, "user", "name", "username", false)
				if name != "" {
					stats[id].Name = &name
				} else {
					stats[id].Name = &emptyDash
				}
			}
		}
	}

	// Calculating the histogram data is expensive, so only do it if needed.
	// An explicit resolver can not be used because we need to know the filters.
	histogramsNeeded := false
	fields := graphql.CollectFieldsCtx(ctx, nil)
	for _, col := range fields {
		if col.Name == "histDuration" || col.Name == "histNumNodes" {
			histogramsNeeded = true
		}
	}

	res := make([]*model.JobsStatistics, 0, len(stats))
	for _, stat := range stats {
		res = append(res, stat)
		id, col := "", ""
		if groupBy != nil {
			id = stat.ID
			col = groupBy2column[*groupBy]
		}

		if histogramsNeeded {
			var err error
			value := fmt.Sprintf(`CAST(ROUND((CASE WHEN job.job_state = "running" THEN %d - job.start_time ELSE job.duration END) / 3600) as %s) as value`, time.Now().Unix(), castType)
			stat.HistDuration, err = r.jobsStatisticsHistogram(ctx, value, filter, id, col)
			if err != nil {
				log.Warn("Error while loading job statistics histogram: running jobs")
				return nil, err
			}

			stat.HistNumNodes, err = r.jobsStatisticsHistogram(ctx, "job.num_nodes as value", filter, id, col)
			if err != nil {
				log.Warn("Error while loading job statistics histogram: num nodes")
				return nil, err
			}
		}
	}

	log.Infof("Timer JobStatistics %s", time.Since(start))
	return res, nil
}

// `value` must be the column grouped by, but renamed to "value". `id` and `col` can optionally be used
// to add a condition to the query of the kind "<col> = <id>".
func (r *JobRepository) jobsStatisticsHistogram(ctx context.Context,
	value string, filters []*model.JobFilter, id, col string) ([]*model.HistoPoint, error) {

	start := time.Now()
	query := sq.Select(value, "COUNT(job.id) AS count").From("job")
	query, qerr := SecurityCheck(ctx, sq.Select(value, "COUNT(job.id) AS count").From("job"))

	if qerr != nil {
		return nil, qerr
	}

	for _, f := range filters {
		query = BuildWhereClause(f, query)
	}

	if len(id) != 0 && len(col) != 0 {
		query = query.Where(col+" = ?", id)
	}

	rows, err := query.GroupBy("value").RunWith(r.DB).Query()
	if err != nil {
		log.Error("Error while running query")
		return nil, err
	}

	points := make([]*model.HistoPoint, 0)
	for rows.Next() {
		point := model.HistoPoint{}
		if err := rows.Scan(&point.Value, &point.Count); err != nil {
			log.Warn("Error while scanning rows")
			return nil, err
		}

		points = append(points, &point)
	}
	log.Infof("Timer jobsStatisticsHistogram %s", time.Since(start))
	return points, nil
}
