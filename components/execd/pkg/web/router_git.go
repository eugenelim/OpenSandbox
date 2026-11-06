// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !windows

package web

import (
	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/web/controller"
)

func registerGitRoutes(r *gin.Engine) {
	git := r.Group("/git")
	{
		git.GET("/capabilities", func(c *gin.Context) { controller.NewGitController(c).Capabilities() })
		git.GET("/refs", func(c *gin.Context) { controller.NewGitController(c).ListRefs() })
		git.POST("/bundle/create", func(c *gin.Context) { controller.NewGitController(c).CreateBundle() })
		git.POST("/bundle/receive", func(c *gin.Context) { controller.NewGitController(c).ReceiveBundle() })
	}
}
