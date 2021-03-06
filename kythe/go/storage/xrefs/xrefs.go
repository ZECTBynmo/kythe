/*
 * Copyright 2015 Google Inc. All rights reserved.
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

// Package xrefs contains a simple implementation of the xrefs.Service interface
// backed by a graphstore.Service.
package xrefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strconv"
	"time"

	"kythe.io/kythe/go/services/graphstore"
	"kythe.io/kythe/go/services/xrefs"
	"kythe.io/kythe/go/util/encoding/text"
	"kythe.io/kythe/go/util/kytheuri"
	"kythe.io/kythe/go/util/schema"
	"kythe.io/kythe/go/util/schema/edges"
	"kythe.io/kythe/go/util/schema/facts"
	"kythe.io/kythe/go/util/schema/nodes"
	"kythe.io/kythe/go/util/schema/tickets"

	"bitbucket.org/creachadair/stringset"

	cpb "kythe.io/kythe/proto/common_proto"
	gpb "kythe.io/kythe/proto/graph_proto"
	spb "kythe.io/kythe/proto/storage_proto"
	xpb "kythe.io/kythe/proto/xref_proto"
)

// EnsureReverseEdges checks if gs contains reverse edges.  If it doesn't, it
// will scan gs for all forward edges, adding a reverse for each back into the
// GraphStore.  This is necessary for a GraphStoreService to work properly.
func EnsureReverseEdges(ctx context.Context, gs graphstore.Service) error {
	var edge *spb.Entry
	if err := gs.Scan(ctx, &spb.ScanRequest{}, func(e *spb.Entry) error {
		if graphstore.IsEdge(e) {
			edge = e
			return io.EOF
		}
		return nil
	}); err != nil {
		return err
	}

	if edge == nil {
		log.Println("No edges found in GraphStore")
		return nil
	} else if edges.IsReverse(edge.EdgeKind) {
		return nil
	}

	var foundReverse bool
	if err := gs.Read(ctx, &spb.ReadRequest{
		Source:   edge.Target,
		EdgeKind: edges.Mirror(edge.EdgeKind),
	}, func(entry *spb.Entry) error {
		foundReverse = true
		return nil
	}); err != nil {
		return fmt.Errorf("error checking for reverse edge: %v", err)
	}
	if foundReverse {
		return nil
	}
	return addReverseEdges(ctx, gs)
}

func addReverseEdges(ctx context.Context, gs graphstore.Service) error {
	log.Println("Adding reverse edges")
	var (
		totalEntries int
		addedEdges   int
	)
	startTime := time.Now()
	err := gs.Scan(ctx, new(spb.ScanRequest), func(entry *spb.Entry) error {
		kind := entry.EdgeKind
		if kind != "" && edges.IsForward(kind) {
			if err := gs.Write(ctx, &spb.WriteRequest{
				Source: entry.Target,
				Update: []*spb.WriteRequest_Update{{
					Target:    entry.Source,
					EdgeKind:  edges.Mirror(kind),
					FactName:  entry.FactName,
					FactValue: entry.FactValue,
				}},
			}); err != nil {
				return fmt.Errorf("Failed to write reverse edge: %v", err)
			}
			addedEdges++
		}
		totalEntries++
		return nil
	})
	log.Printf("Wrote %d reverse edges to GraphStore (%d total entries): %v", addedEdges, totalEntries, time.Since(startTime))
	return err
}

// A GraphStoreService partially implements the xrefs.Service interface
// directly using a graphstore.Service with stored reverse edges.  This is a
// low-performance, simple alternative to creating the serving Table
// representation.
// TODO(schroederc): parallelize GraphStore calls
type GraphStoreService struct {
	gs graphstore.Service
}

// NewGraphStoreService returns a new GraphStoreService given an
// existing graphstore.Service.
func NewGraphStoreService(gs graphstore.Service) *GraphStoreService {
	return &GraphStoreService{gs}
}

// Nodes implements part of the Service interface.
func (g *GraphStoreService) Nodes(ctx context.Context, req *gpb.NodesRequest) (*gpb.NodesReply, error) {
	patterns := xrefs.ConvertFilters(req.Filter)

	var names []*spb.VName
	for _, ticket := range req.Ticket {
		name, err := kytheuri.ToVName(ticket)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	nodes := make(map[string]*cpb.NodeInfo)
	for i, vname := range names {
		ticket := req.Ticket[i]
		info := &cpb.NodeInfo{Facts: make(map[string][]byte)}
		if err := g.gs.Read(ctx, &spb.ReadRequest{Source: vname}, func(entry *spb.Entry) error {
			if len(patterns) == 0 || xrefs.MatchesAny(entry.FactName, patterns) {
				info.Facts[entry.FactName] = entry.FactValue
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if len(info.Facts) > 0 {
			nodes[ticket] = info
		}
	}
	return &gpb.NodesReply{Nodes: nodes}, nil
}

// Edges implements part of the Service interface.
func (g *GraphStoreService) Edges(ctx context.Context, req *gpb.EdgesRequest) (*gpb.EdgesReply, error) {
	if len(req.Ticket) == 0 {
		return nil, errors.New("no tickets specified")
	} else if req.PageToken != "" {
		return nil, errors.New("UNIMPLEMENTED: page_token")
	}

	patterns := xrefs.ConvertFilters(req.Filter)
	allowedKinds := stringset.New(req.Kind...)
	var targetSet stringset.Set
	reply := &gpb.EdgesReply{
		EdgeSets: make(map[string]*gpb.EdgeSet),
		Nodes:    make(map[string]*cpb.NodeInfo),
	}

	for _, ticket := range req.Ticket {
		vname, err := kytheuri.ToVName(ticket)
		if err != nil {
			return nil, fmt.Errorf("invalid ticket %q: %v", ticket, err)
		}

		var (
			// EdgeKind -> TargetTicket -> OrdinalSet
			filteredEdges = make(map[string]map[string]map[int32]struct{})
			filteredFacts = make(map[string][]byte)
		)

		if err := g.gs.Read(ctx, &spb.ReadRequest{
			Source:   vname,
			EdgeKind: "*",
		}, func(entry *spb.Entry) error {
			edgeKind := entry.EdgeKind
			if edgeKind == "" {
				// node fact
				if len(patterns) > 0 && xrefs.MatchesAny(entry.FactName, patterns) {
					filteredFacts[entry.FactName] = entry.FactValue
				}
			} else {
				// edge
				edgeKind, ordinal, _ := edges.ParseOrdinal(edgeKind)
				if len(req.Kind) == 0 || allowedKinds.Contains(edgeKind) {
					targets, ok := filteredEdges[edgeKind]
					if !ok {
						targets = make(map[string]map[int32]struct{})
						filteredEdges[edgeKind] = targets
					}
					ticket := kytheuri.ToString(entry.Target)
					ordSet, ok := targets[ticket]
					if !ok {
						ordSet = make(map[int32]struct{})
						targets[ticket] = ordSet
					}
					ordSet[int32(ordinal)] = struct{}{}
				}
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("failed to retrieve entries for ticket %q", ticket)
		}

		// Only add a EdgeSet if there are targets for the requested edge kinds.
		if len(filteredEdges) > 0 {
			groups := make(map[string]*gpb.EdgeSet_Group)
			for edgeKind, targets := range filteredEdges {
				g := &gpb.EdgeSet_Group{}
				for target, ordinals := range targets {
					for ordinal := range ordinals {
						g.Edge = append(g.Edge, &gpb.EdgeSet_Group_Edge{
							TargetTicket: target,
							Ordinal:      ordinal,
						})
					}
					targetSet.Add(target)
				}
				groups[edgeKind] = g
			}
			reply.EdgeSets[ticket] = &gpb.EdgeSet{
				Groups: groups,
			}

			// In addition, only add a NodeInfo if the filters have resulting facts.
			if len(filteredFacts) > 0 {
				reply.Nodes[ticket] = &cpb.NodeInfo{
					Facts: filteredFacts,
				}
			}
		}
	}

	// Only request Nodes when there are fact filters given.
	if len(req.Filter) > 0 {
		// Eliminate redundant work by removing already requested nodes from targetSet
		for ticket := range reply.Nodes {
			targetSet.Discard(ticket)
		}

		// Batch request all leftover target nodes
		nodesReply, err := g.Nodes(ctx, &gpb.NodesRequest{
			Ticket: targetSet.Elements(),
			Filter: req.Filter,
		})
		if err != nil {
			return nil, fmt.Errorf("failure getting target nodes: %v", err)
		}
		for ticket, node := range nodesReply.Nodes {
			reply.Nodes[ticket] = node
		}
	}

	return reply, nil
}

// Decorations implements part of the Service interface.
func (g *GraphStoreService) Decorations(ctx context.Context, req *xpb.DecorationsRequest) (*xpb.DecorationsReply, error) {
	if len(req.DirtyBuffer) > 0 {
		return nil, errors.New("UNIMPLEMENTED: dirty buffers")
	} else if req.GetLocation() == nil {
		// TODO(schroederc): allow empty location when given dirty buffer
		return nil, errors.New("missing location")
	}

	fileVName, err := kytheuri.ToVName(req.Location.Ticket)
	if err != nil {
		return nil, fmt.Errorf("invalid file ticket %q: %v", req.Location.Ticket, err)
	}

	text, encoding, err := getSourceText(ctx, g.gs, fileVName)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve file text: %v", err)
	}
	norm := xrefs.NewNormalizer(text)

	loc, err := norm.Location(req.GetLocation())
	if err != nil {
		return nil, err
	}

	reply := &xpb.DecorationsReply{
		Location: loc,
		Nodes:    make(map[string]*cpb.NodeInfo),
	}

	// Handle DecorationsRequest.SourceText switch
	if req.SourceText {
		if loc.Kind == xpb.Location_FILE {
			reply.SourceText = text
		} else {
			reply.SourceText = text[loc.Start.ByteOffset:loc.End.ByteOffset]
		}
		reply.Encoding = encoding
	}

	// Handle DecorationsRequest.References switch
	if req.References {
		// Traverse the following chain of edges:
		//   file --%/kythe/edge/childof-> []anchor --forwardEdgeKind-> []target
		//
		// Add []anchor and []target nodes to reply.Nodes
		// Add all {anchor, forwardEdgeKind, target} tuples to reply.Reference

		patterns := xrefs.ConvertFilters(req.Filter)

		children, err := getEdges(ctx, g.gs, fileVName, func(e *spb.Entry) bool {
			return e.EdgeKind == revChildOfEdgeKind
		})
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve file children: %v", err)
		}

		var targetSet stringset.Set
		for _, edge := range children {
			anchor := edge.Target
			ticket := kytheuri.ToString(anchor)
			anchorNodeReply, err := g.Nodes(ctx, &gpb.NodesRequest{
				Ticket: []string{ticket},
			})
			if err != nil {
				return nil, fmt.Errorf("failure getting reference source node: %v", err)
			} else if len(anchorNodeReply.Nodes) != 1 {
				return nil, fmt.Errorf("found %d nodes for {%+v}", len(anchorNodeReply.Nodes), anchor)
			}

			node, ok := xrefs.NodesMap(anchorNodeReply.Nodes)[ticket]
			if !ok {
				return nil, fmt.Errorf("failed to find info for node %q", ticket)
			} else if string(node[facts.NodeKind]) != nodes.Anchor {
				// Skip child if it isn't an anchor node
				continue
			}

			anchorStart, err := strconv.Atoi(string(node[facts.AnchorStart]))
			if err != nil {
				log.Printf("Invalid anchor start offset %q for node %q: %v", node[facts.AnchorStart], ticket, err)
				continue
			}
			anchorEnd, err := strconv.Atoi(string(node[facts.AnchorEnd]))
			if err != nil {
				log.Printf("Invalid anchor end offset %q for node %q: %v", node[facts.AnchorEnd], ticket, err)
				continue
			}

			if loc.Kind == xpb.Location_SPAN {
				// Check if anchor fits within/around requested source text window
				if !xrefs.InSpanBounds(req.SpanKind, int32(anchorStart), int32(anchorEnd), loc.Start.ByteOffset, loc.End.ByteOffset) {
					continue
				} else if anchorStart > anchorEnd {
					log.Printf("Invalid anchor offset span %d:%d", anchorStart, anchorEnd)
					continue
				}
			}

			targets, err := getEdges(ctx, g.gs, anchor, func(e *spb.Entry) bool {
				return edges.IsForward(e.EdgeKind) && e.EdgeKind != edges.ChildOf
			})
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve targets of anchor %v: %v", anchor, err)
			}
			if len(targets) == 0 {
				log.Printf("Anchor missing forward edges: {%+v}", anchor)
				continue
			}

			if node := filterNode(patterns, anchorNodeReply.Nodes[ticket]); node != nil {
				reply.Nodes[ticket] = node
			}
			for _, edge := range targets {
				targetTicket := kytheuri.ToString(edge.Target)
				targetSet.Add(targetTicket)
				reply.Reference = append(reply.Reference, &xpb.DecorationsReply_Reference{
					SourceTicket: ticket,
					Kind:         edge.Kind,
					TargetTicket: targetTicket,
					AnchorStart:  norm.ByteOffset(int32(anchorStart)),
					AnchorEnd:    norm.ByteOffset(int32(anchorEnd)),
				})
			}
		}
		sort.Sort(bySpan(reply.Reference))

		// Only request Nodes when there are fact filters given.
		if len(req.Filter) > 0 {
			// Ensure returned nodes are not duplicated.
			for ticket := range reply.Nodes {
				targetSet.Discard(ticket)
			}

			// Batch request all Reference target nodes
			nodesReply, err := g.Nodes(ctx, &gpb.NodesRequest{
				Ticket: targetSet.Elements(),
				Filter: req.Filter,
			})
			if err != nil {
				return nil, fmt.Errorf("failure getting reference target nodes: %v", err)
			}
			for ticket, node := range nodesReply.Nodes {
				reply.Nodes[ticket] = node
			}
		}
	}

	return reply, nil
}

var revChildOfEdgeKind = edges.Mirror(edges.ChildOf)

func getSourceText(ctx context.Context, gs graphstore.Service, fileVName *spb.VName) (text []byte, encoding string, err error) {
	if err := gs.Read(ctx, &spb.ReadRequest{Source: fileVName}, func(entry *spb.Entry) error {
		switch entry.FactName {
		case facts.Text:
			text = entry.FactValue
		case facts.TextEncoding:
			encoding = string(entry.FactValue)
		default:
			// skip other file facts
		}
		return nil
	}); err != nil {
		return nil, "", fmt.Errorf("read error: %v", err)
	}
	if text == nil {
		err = fmt.Errorf("file not found: %+v", fileVName)
	}
	return
}

type edgeTarget struct {
	Kind    string
	Target  *spb.VName
	Ordinal int32
}

// getEdges returns edgeTargets with the given node as their source.  Only edge
// entries that return true when applied to pred are returned.
func getEdges(ctx context.Context, gs graphstore.Service, node *spb.VName, pred func(*spb.Entry) bool) ([]*edgeTarget, error) {
	var targets []*edgeTarget

	if err := gs.Read(ctx, &spb.ReadRequest{
		Source:   node,
		EdgeKind: "*",
	}, func(entry *spb.Entry) error {
		if graphstore.IsEdge(entry) && pred(entry) {
			edgeKind, ordinal, _ := edges.ParseOrdinal(entry.EdgeKind)
			targets = append(targets, &edgeTarget{edgeKind, entry.Target, int32(ordinal)})
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read error: %v", err)
	}
	return targets, nil
}

func filterNode(patterns []*regexp.Regexp, node *cpb.NodeInfo) *cpb.NodeInfo {
	if len(patterns) == 0 {
		return nil
	}

	filteredFacts := make(map[string][]byte)
	for name, value := range node.Facts {
		if xrefs.MatchesAny(name, patterns) {
			filteredFacts[name] = value
		}
	}

	if len(filteredFacts) == 0 {
		return nil
	}
	return &cpb.NodeInfo{
		Facts: filteredFacts,
	}
}

// bySpan implements the sort.Interface, ordering by each reference's anchor
// span.
type bySpan []*xpb.DecorationsReply_Reference

// Len implements part of the sort.Interface.
func (s bySpan) Len() int { return len(s) }

// Swap implements part of the sort.Interface.
func (s bySpan) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// Less implements part of the sort.Interface.
func (s bySpan) Less(i, j int) bool {
	if s[i].AnchorStart.ByteOffset < s[j].AnchorStart.ByteOffset {
		return true
	} else if s[i].AnchorStart.ByteOffset > s[j].AnchorStart.ByteOffset {
		return false
	} else if s[i].AnchorEnd.ByteOffset < s[j].AnchorEnd.ByteOffset {
		return true
	}
	return false
}

const defaultXRefPageSize = 1024

// CrossReferences implements part of the xrefs Service interface.
func (g *GraphStoreService) CrossReferences(ctx context.Context, req *xpb.CrossReferencesRequest) (*xpb.CrossReferencesReply, error) {
	// TODO(zarko): Callgraph integration.
	if len(req.Ticket) == 0 {
		return nil, errors.New("no cross-references requested")
	}

	requestedPageSize := int(req.PageSize)
	if requestedPageSize == 0 {
		requestedPageSize = defaultXRefPageSize
	}

	eReply, err := g.Edges(ctx, &gpb.EdgesRequest{
		Ticket:    req.Ticket,
		PageSize:  int32(requestedPageSize),
		PageToken: req.PageToken,
	})
	if err != nil {
		return nil, fmt.Errorf("error getting edges for cross-references: %v", err)
	}

	reply := &xpb.CrossReferencesReply{
		CrossReferences: make(map[string]*xpb.CrossReferencesReply_CrossReferenceSet),

		NextPageToken: eReply.NextPageToken,
	}
	var allRelatedNodes stringset.Set
	if len(req.Filter) > 0 {
		reply.Nodes = make(map[string]*cpb.NodeInfo)
	}

	// Cache parent files across all anchors
	files := make(map[string]*fileNode)

	var totalXRefs int
	for {
		for source, es := range eReply.EdgeSets {
			xr, ok := reply.CrossReferences[source]
			if !ok {
				xr = &xpb.CrossReferencesReply_CrossReferenceSet{Ticket: source}
			}

			var count int
			for kind, grp := range es.Groups {
				switch {
				// TODO(schroeder): handle declarations
				case xrefs.IsDefKind(req.DefinitionKind, kind, false):
					anchors, err := completeAnchors(ctx, g, req.AnchorText, files, kind, edgeTickets(grp.Edge))
					if err != nil {
						return nil, fmt.Errorf("error resolving definition anchors: %v", err)
					}
					count += len(anchors)
					xr.Definition = append(xr.Definition, anchors...)
				case xrefs.IsRefKind(req.ReferenceKind, kind):
					anchors, err := completeAnchors(ctx, g, req.AnchorText, files, kind, edgeTickets(grp.Edge))
					if err != nil {
						return nil, fmt.Errorf("error resolving reference anchors: %v", err)
					}
					count += len(anchors)
					xr.Reference = append(xr.Reference, anchors...)
				case xrefs.IsDocKind(req.DocumentationKind, kind):
					anchors, err := completeAnchors(ctx, g, req.AnchorText, files, kind, edgeTickets(grp.Edge))
					if err != nil {
						return nil, fmt.Errorf("error resolving documentation anchors: %v", err)
					}
					count += len(anchors)
					xr.Documentation = append(xr.Documentation, anchors...)
				case !allRelatedNodes.Empty() && !edges.IsAnchorEdge(kind):
					count += len(grp.Edge)
					for _, edge := range grp.Edge {
						xr.RelatedNode = append(xr.RelatedNode, &xpb.CrossReferencesReply_RelatedNode{
							Ticket:       edge.TargetTicket,
							RelationKind: kind,
							Ordinal:      edge.Ordinal,
						})
						allRelatedNodes.Add(edge.TargetTicket)
					}
				}
			}

			if count > 0 {
				reply.CrossReferences[xr.Ticket] = xr
				totalXRefs += count
			}
		}

		if reply.NextPageToken == "" || totalXRefs > 0 {
			break
		}

		// We need to return at least 1 xref, if there are any
		log.Println("Extra CrossReferences Edges call: ", reply.NextPageToken)
		eReply, err = g.Edges(ctx, &gpb.EdgesRequest{
			Ticket:    req.Ticket,
			PageSize:  int32(requestedPageSize),
			PageToken: reply.NextPageToken,
		})
		if err != nil {
			return nil, fmt.Errorf("error getting edges for cross-references: %v", err)
		}
		reply.NextPageToken = eReply.NextPageToken
	}

	if !allRelatedNodes.Empty() {
		nReply, err := g.Nodes(ctx, &gpb.NodesRequest{
			Ticket: allRelatedNodes.Elements(),
			Filter: req.Filter,
		})
		if err != nil {
			return nil, fmt.Errorf("error retrieving related nodes: %v", err)
		}
		for ticket, n := range nReply.Nodes {
			reply.Nodes[ticket] = n
		}
	}

	return reply, nil
}

type fileNode struct {
	text     []byte
	encoding string
	norm     *xrefs.Normalizer
}

func edgeTickets(edges []*gpb.EdgeSet_Group_Edge) (tickets []string) {
	for _, e := range edges {
		tickets = append(tickets, e.TargetTicket)
	}
	return
}

func completeAnchors(ctx context.Context, xs xrefs.GraphService, retrieveText bool, files map[string]*fileNode, edgeKind string, anchors []string) ([]*xpb.CrossReferencesReply_RelatedAnchor, error) {
	edgeKind = edges.Canonical(edgeKind)

	parents := make(map[string]string)
	for _, anchor := range anchors {
		file, err := tickets.AnchorFile(anchor)
		if err != nil {
			return nil, fmt.Errorf("invalid anchor %q: %v", anchor, err)
		}
		parents[anchor] = file
	}
	reply, err := xs.Nodes(ctx, &gpb.NodesRequest{
		Ticket: anchors,
		Filter: []string{
			schema.AnchorLocFilter,
			schema.SnippetLocFilter,
		},
	})
	if err != nil {
		return nil, err
	}

	var result []*xpb.CrossReferencesReply_RelatedAnchor
	for ticket, info := range reply.Nodes {
		start, end, err := getSpan(info.Facts, facts.AnchorStart, facts.AnchorEnd)
		if err != nil {
			log.Printf("Invalid anchor span for %q: %v", ticket, err)
			continue
		}

		// Add this anchor to the result for its parent file.
		anchor := &xpb.Anchor{
			Ticket: ticket,
			Kind:   edgeKind,
			Parent: parents[ticket],
		}

		// If we haven't already fetched the contents of this file, do so now.
		file, ok := files[anchor.Parent]
		if !ok {
			rsp, err := xs.Nodes(ctx, &gpb.NodesRequest{
				Ticket: []string{anchor.Parent},
			})
			if err != nil {
				return nil, fmt.Errorf("fetching file contents for %q: %v", anchor.Parent, err)
			}
			info := rsp.Nodes[anchor.Parent]
			text := info.Facts[facts.Text]
			file = &fileNode{
				text:     text,
				encoding: string(info.Facts[facts.TextEncoding]),
				norm:     xrefs.NewNormalizer(text),
			}
			files[anchor.Parent] = file
		}

		// Normalize the anchor's bounds relative to the file.
		anchor.Start, anchor.End, err = normalizeSpan(file.norm, int32(start), int32(end))
		if err != nil {
			log.Printf("Invalid anchor span %q in file %q: %v", ticket, anchor.Parent, err)
			continue
		}

		// Decode the content of the file spanned by the anchor.
		if retrieveText && anchor.Start.ByteOffset < anchor.End.ByteOffset {
			anchor.Text, err = text.ToUTF8(file.encoding, file.text[anchor.Start.ByteOffset:anchor.End.ByteOffset])
			if err != nil {
				log.Printf("Error decoding anchor text: %v", err)
			}
		}

		// If the anchor provided snippet bounds, extract the snippet.
		if snipStart, snipEnd, err := getSpan(reply.Nodes[ticket].Facts, facts.SnippetStart, facts.SnippetEnd); err == nil {
			start, end, err := normalizeSpan(file.norm, int32(snipStart), int32(snipEnd))
			if err != nil {
				log.Printf("Invalid snippet span %q in file %q: %v", ticket, anchor.Parent, err)
			} else {
				anchor.Snippet, err = text.ToUTF8(file.encoding, file.text[start.ByteOffset:end.ByteOffset])
				if err != nil {
					log.Printf("Error decoding snippet text: %v", err)
				}
				anchor.SnippetStart = start
				anchor.SnippetEnd = end
			}
		}

		// Fall back to a line-based snippet if the indexer did not provide its
		// own snippet offsets.
		if anchor.Snippet == "" {
			anchor.SnippetStart = &xpb.Location_Point{
				ByteOffset: anchor.Start.ByteOffset - anchor.Start.ColumnOffset,
				LineNumber: anchor.Start.LineNumber,
			}
			nextLine := file.norm.Point(&xpb.Location_Point{LineNumber: anchor.Start.LineNumber + 1})
			anchor.SnippetEnd = &xpb.Location_Point{
				ByteOffset:   nextLine.ByteOffset - 1,
				LineNumber:   anchor.Start.LineNumber,
				ColumnOffset: anchor.Start.ColumnOffset + (nextLine.ByteOffset - anchor.Start.ByteOffset - 1),
			}
			anchor.Snippet, err = text.ToUTF8(file.encoding,
				file.text[anchor.SnippetStart.ByteOffset:anchor.SnippetEnd.ByteOffset])
			if err != nil {
				log.Printf("Error decoding snippet text: %v", err)
			}
		}

		result = append(result, &xpb.CrossReferencesReply_RelatedAnchor{Anchor: anchor})
	}
	return result, nil
}

func getSpan(facts map[string][]byte, startFact, endFact string) (startOffset, endOffset int, err error) {
	start := string(facts[startFact])
	end := string(facts[endFact])
	if start == "" || end == "" {
		return 0, 0, fmt.Errorf("missing location facts; found: %s=%q and %s=%q",
			startFact, start, endFact, end)
	}
	so, err := strconv.Atoi(start)
	if err != nil {
		return 0, 0, fmt.Errorf("error parsing %s value %q: %v", startFact, start, err)
	}
	eo, err := strconv.Atoi(end)
	if err != nil {
		return 0, 0, fmt.Errorf("error parsing %s value %q: %v", endFact, end, err)
	}
	if so > eo {
		return 0, 0, fmt.Errorf("invalid %s/%s span: %d-%d", startFact, endFact, so, eo)
	}

	return so, eo, nil
}

func normalizeSpan(norm *xrefs.Normalizer, startOffset, endOffset int32) (start, end *xpb.Location_Point, err error) {
	start = norm.ByteOffset(startOffset)
	end = norm.ByteOffset(endOffset)

	if start.ByteOffset != startOffset {
		err = fmt.Errorf("inconsistent start location; expected: %d; found; %d",
			startOffset, start.ByteOffset)
	} else if end.ByteOffset != endOffset {
		err = fmt.Errorf("inconsistent end location; expected: %d; found; %d",
			endOffset, end.ByteOffset)
	}
	return
}

// Documentation implements part of the Service interface.
func (g *GraphStoreService) Documentation(ctx context.Context, req *xpb.DocumentationRequest) (*xpb.DocumentationReply, error) {
	return xrefs.SlowDocumentation(ctx, g, req)
}
