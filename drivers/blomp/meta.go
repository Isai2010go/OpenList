package blomp

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootPath
	Tenant         string `json:"tenant" type:"string" default:"storage" help:"Swift tenant/project name; confirm this value with Blomp support"`
	Username       string `json:"username" type:"string" required:"true" help:"Blomp account email address"`
	Password       string `json:"password" type:"string" required:"true" help:"Blomp account password"`
	AuthURL        string `json:"auth_url" type:"string" required:"true" help:"Keystone v2 authentication URL supplied by Blomp"`
	Region         string `json:"region" type:"string" help:"Optional Swift region name returned by Blomp Keystone"`
	AllowHTTP      bool   `json:"allow_http" type:"bool" default:"false" help:"Allow the HTTP storage endpoint returned by Blomp. The token and file data will be unencrypted."`
}

var config = driver.Config{
	Name:        "Blomp",
	DefaultRoot: "",
	OnlyProxy:   true,
	NoCache:     false,
	NoUpload:    false,
	Alert:       "Blomp uses Swift API. Its current public storage endpoint may use plain HTTP; enable Allow HTTP only if you accept the risk.",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Blomp{}
	})
}
