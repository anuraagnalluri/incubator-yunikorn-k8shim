// +build graphviz

/*
 Licensed to the Apache Software Foundation (ASF) under one
 or more contributor license agreements.  See the NOTICE file
 distributed with this work for additional information
 regarding copyright ownership.  The ASF licenses this file
 to you under the Apache License, Version 2.0 (the
 "License"); you may not use this file except in compliance
 with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package cache

import (
	"io/ioutil"
	"os"
	"testing"

	"github.com/looplab/fsm"
	"gotest.tools/assert"

	"github.com/apache/incubator-yunikorn-k8shim/pkg/common"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/constants"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/test"
)

func TestNodeFsmGraph(t *testing.T) {
	api := test.NewSchedulerAPIMock()
	r1 := common.NewResourceBuilder().
		AddResource(constants.Memory, 1).
		AddResource(constants.CPU, 1).
		Build()
	node := newSchedulerNode("host001", "UID001", "{}", r1, api, false)
	graph := fsm.Visualize(node.fsm)

	err := os.MkdirAll("../../_output/fsm", 0755)
	assert.NilError(t, err, "Creating output dir failed")
	ioutil.WriteFile("../../_output/fsm/k8shim-node-state.dot", []byte(graph), 0644)
	assert.NilError(t, err, "Writing graph failed")
}
