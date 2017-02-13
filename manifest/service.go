package manifest

import (
	"crypto/sha1"
	"fmt"
)

type Service struct {
	Name string

	Build       ServiceBuild
	Environment []string
	Image       string
	Test        string
}

type Services []Service

type ServiceBuild struct {
	Args []string
	Path string
}

func (s Service) BuildHash() string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("build[path=%q, args=%v] image=%q", s.Build.Path, s.Build.Args, s.Image))))
}
