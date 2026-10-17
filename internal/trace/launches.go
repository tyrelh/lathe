package trace

import (
	"encoding/json"
	"errors"
)

// Launch states.
const (
	LaunchIntent     = "intent"
	LaunchRegistered = "registered"
	LaunchDrained    = "drained"
	LaunchCancelled  = "cancelled"
)

// openLaunch matches a launch not yet drained or cancelled.
const openLaunch = "state IN ('" + LaunchIntent + "', '" + LaunchRegistered + "')"

// ErrLaunchRevoked is a registration the run no longer admits.
var ErrLaunchRevoked = errors.New("the launch is no longer admitted")

// Launch is one supervised subprocess as recovery reads it.
type Launch struct {
	ID        string
	RunID     string
	AttemptID string
	State     string
	Host      string
	PID       int
	PGID      int
	Endpoint  string
	Token     string
}

// RecordLaunchIntent records that attemptID is about to start a supervised
// subprocess. It lands only while the attempt owns the run.
func (d *DB) RecordLaunchIntent(runID, attemptID, launchID string, argv []string) error {
	b, _ := json.Marshal(argv)
	res, err := d.sql.Exec(
		`INSERT INTO launches (launch_id, run_id, attempt_id, state, argv, intent_at)
		 SELECT ?, ?, ?, 'intent', ?, ? WHERE `+owned,
		launchID, runID, attemptID, string(b), nowUTC(), runID, attemptID)
	if err != nil {
		return err
	}
	return claimed(res)
}

// RegisterLaunch records a started, not yet admitted, supervisor. It lands
// only for an intent whose attempt still owns its run, so a run settled or
// an intent cancelled meanwhile is never admitted: that is ErrLaunchRevoked.
func (d *DB) RegisterLaunch(launchID, host string, pid, pgid int, endpoint, token string) error {
	res, err := d.sql.Exec(
		`UPDATE launches SET state = 'registered', host = ?, pid = ?, pgid = ?, endpoint = ?, token = ?, registered_at = ?
		 WHERE launch_id = ? AND state = 'intent'
		   AND EXISTS (SELECT 1 FROM runs r WHERE r.run_id = launches.run_id
		     AND r.attempt_id = launches.attempt_id AND r.`+executing+`)`,
		host, pid, pgid, endpoint, token, nowUTC(), launchID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrLaunchRevoked
	}
	return nil
}

// DrainLaunch records that a launch's process group has stopped.
func (d *DB) DrainLaunch(launchID string) error {
	_, err := d.sql.Exec(
		`UPDATE launches SET state = 'drained', drained_at = ? WHERE launch_id = ? AND `+openLaunch,
		nowUTC(), launchID)
	return err
}

// CancelLaunch revokes an intent that was never registered, and reports
// whether it did. A registered launch is left for its group to be stopped.
func (d *DB) CancelLaunch(launchID string) (bool, error) {
	res, err := d.sql.Exec(
		`UPDATE launches SET state = 'cancelled', drained_at = ? WHERE launch_id = ? AND state = 'intent'`,
		nowUTC(), launchID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// OpenLaunches is every launch of runID not yet drained or cancelled.
func (d *DB) OpenLaunches(runID string) ([]Launch, error) {
	rows, err := d.sql.Query(
		`SELECT launch_id, run_id, attempt_id, state, COALESCE(host, ''), pid, pgid,
		        COALESCE(endpoint, ''), COALESCE(token, '')
		 FROM launches WHERE run_id = ? AND `+openLaunch+` ORDER BY intent_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Launch
	for rows.Next() {
		var l Launch
		if err := rows.Scan(&l.ID, &l.RunID, &l.AttemptID, &l.State, &l.Host, &l.PID, &l.PGID,
			&l.Endpoint, &l.Token); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
