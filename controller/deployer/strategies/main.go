package strategy

import (
	"fmt"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
	"github.com/flynn/flynn/controller/client"
	ct "github.com/flynn/flynn/controller/types"
)

type PerformFunc func(log15.Logger, *controller.Client, *ct.Deployment, chan<- ct.DeploymentEvent) error

var performFuncs = map[string]PerformFunc{
	"all-at-once": allAtOnce,
	"one-by-one":  oneByOne,
}

func Get(strategy string) (PerformFunc, error) {
	if f, ok := performFuncs[strategy]; ok {
		return f, nil
	}
	return nil, fmt.Errorf("Unknown strategy '%s'!", strategy)
}

// TODO: share with tests
func jobEventsEqual(expected, actual jobEvents) bool {
	for rel, m := range expected {
		j, ok := actual[rel]
		if !ok {
			return false
		}
		for typ, events := range m {
			diff, ok := j[typ]
			if !ok {
				return false
			}
			for state, count := range events {
				if diff[state] != count {
					return false
				}
			}
		}
	}
	return true
}

type jobEvents map[string]map[string]map[string]int

func waitForJobEvents(events chan *ct.JobEvent, deployEvents chan<- ct.DeploymentEvent, expected jobEvents) error {
	fmt.Printf("waiting for job events: %v\n", expected)
	actual := make(jobEvents)
	for {
	inner:
		select {
		case event := <-events:
			fmt.Printf("got job event: %s %s %s\n", event.Type, event.JobID, event.State)
			if _, ok := actual[event.Job.ReleaseID]; !ok {
				actual[event.Job.ReleaseID] = make(map[string]map[string]int)
			}
			if _, ok := actual[event.Job.ReleaseID][event.Type]; !ok {
				actual[event.Job.ReleaseID][event.Type] = make(map[string]int)
			}
			switch event.State {
			case "up":
				actual[event.Job.ReleaseID][event.Type]["up"] += 1
				deployEvents <- ct.DeploymentEvent{
					ReleaseID: event.Job.ReleaseID,
					JobState:  "up",
					JobType:   event.Type,
				}
			case "down":
				actual[event.Job.ReleaseID][event.Type]["down"] += 1
				deployEvents <- ct.DeploymentEvent{
					ReleaseID: event.Job.ReleaseID,
					JobState:  "down",
					JobType:   event.Type,
				}
			case "crashed":
				actual[event.Job.ReleaseID][event.Type]["crashed"] += 1
				deployEvents <- ct.DeploymentEvent{
					ReleaseID: event.Job.ReleaseID,
					JobState:  "crashed",
					JobType:   event.Type,
				}
				return fmt.Errorf("job crashed!")
			default:
				break inner
			}
			if jobEventsEqual(expected, actual) {
				return nil
			}
		case <-time.After(60 * time.Second):
			return fmt.Errorf("timed out waiting for job events: ", expected)
		}
	}
}
