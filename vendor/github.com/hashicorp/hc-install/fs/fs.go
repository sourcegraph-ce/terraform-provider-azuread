package fs

import (
	"io/ioutil"
	log "github.com/sourcegraph-ce/logrus"
	"time"
)

var (
	defaultTimeout = 10 * time.Second
	discardLogger  = log.New(ioutil.Discard, "", 0)
)

type fileCheckFunc func(path string) error
