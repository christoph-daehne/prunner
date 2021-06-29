package taskctl

import (
	"fmt"
	"io"
	"os"
	"path"

	"github.com/friendsofgo/errors"
)

type OutputStore struct {
	basePath string
}

func NewOutputStore(basePath string) (*OutputStore, error) {
	err := os.MkdirAll(path.Join(basePath, "logs"), 0777)
	if err != nil {
		return nil, errors.Wrap(err, "creating base directory")
	}

	return &OutputStore{
		basePath: basePath,
	}, nil
}

func (s *OutputStore) Writer(jobID string, taskName string, outputName string) (io.WriteCloser, error) {
	err := os.MkdirAll(path.Join(s.basePath, "logs", jobID), 0777)
	if err != nil {
		return nil, errors.Wrap(err, "creating job logs directory")
	}

	filename := s.buildPath(jobID, taskName, outputName)
	f, err := os.Create(filename)
	if err != nil {
		return nil, errors.Wrap(err, "creating task output log file")
	}
	return f, nil
}

func (s *OutputStore) Reader(jobID string, taskName string, outputName string) (io.ReadCloser, error) {
	filename := s.buildPath(jobID, taskName, outputName)
	f, err := os.Open(filename)
	if err != nil {
		return nil, errors.Wrap(err, "opening task output log file")
	}
	return f, nil
}

func (s *OutputStore) buildPath(jobID string, taskName string, outputName string) string {
	return path.Join(s.basePath, "logs", jobID, fmt.Sprintf("%s-%s.log", taskName, outputName))
}