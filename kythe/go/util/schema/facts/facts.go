/*
 * Copyright 2016 Google Inc. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package facts defines constants for Kythe facts.
package facts

const prefix = "/kythe/" // duplicated to avoid a circular import

// Node fact labels
const (
	AnchorEnd    = prefix + "loc/end"
	AnchorStart  = prefix + "loc/start"
	Complete     = prefix + "complete"
	Format       = prefix + "format"
	NodeKind     = prefix + "node/kind"
	SnippetEnd   = prefix + "snippet/end"
	SnippetStart = prefix + "snippet/start"
	Subkind      = prefix + "subkind"
	Text         = prefix + "text"
	TextEncoding = prefix + "text/encoding"
)

// DefaultTextEncoding is the implicit value for TextEncoding if it is empty or
// missing from a node with a Text fact.
const DefaultTextEncoding = "UTF-8"
