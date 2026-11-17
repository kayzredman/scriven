package awscloud

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cttypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
)

// WriteEvent is one CloudTrail record of a call that changed something.
type WriteEvent struct {
	ID     string
	Name   string // API call, e.g. AuthorizeSecurityGroupIngress
	Source string // service, e.g. ec2.amazonaws.com
	At     time.Time
	Who    string // the identity that made the call, e.g. user/alice
	Agent  string // the caller's user agent: console, Terraform, aws-cli ...
	// Values holds every identifier-like string in the request and response
	// (and the resources CloudTrail lists), for matching the event to the
	// resources whose code changed.
	Values []string
}

// Services whose write calls can change a resource scriven captures.
var watchedSources = map[string]bool{
	"ec2.amazonaws.com": true, "elasticloadbalancing.amazonaws.com": true, "s3.amazonaws.com": true,
	"iam.amazonaws.com": true, "dynamodb.amazonaws.com": true, "sns.amazonaws.com": true,
	"rds.amazonaws.com": true, "lambda.amazonaws.com": true, "kms.amazonaws.com": true,
}

// WriteEvents returns write calls to watched services made since the given
// time, oldest first, from CloudTrail's event history (90 days of management
// events, kept by every account at no charge; no trail is needed). IAM calls
// are recorded in us-east-1, so with global set that region is read too.
func (c *Client) WriteEvents(ctx context.Context, since time.Time, global bool) ([]WriteEvent, error) {
	regions := []string{c.Region}
	if global && c.Region != "us-east-1" && c.Endpoint == "" {
		regions = append(regions, "us-east-1")
	}
	var out []WriteEvent
	for i, r := range regions {
		ct := cloudtrail.NewFromConfig(c.cfg, func(o *cloudtrail.Options) { o.Region = r })
		p := cloudtrail.NewLookupEventsPaginator(ct, &cloudtrail.LookupEventsInput{
			StartTime: aws.Time(since),
			LookupAttributes: []cttypes.LookupAttribute{
				{AttributeKey: cttypes.LookupAttributeKeyReadOnly, AttributeValue: aws.String("false")},
			},
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, e := range page.Events {
				ev, ok := parseEvent(e)
				// From the extra region, only IAM: the rest belongs to that region's target.
				if !ok || (i > 0 && ev.Source != "iam.amazonaws.com") {
					continue
				}
				out = append(out, ev)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func parseEvent(e cttypes.Event) (WriteEvent, bool) {
	ev := WriteEvent{ID: aws.ToString(e.EventId), Name: aws.ToString(e.EventName), Source: aws.ToString(e.EventSource),
		At: aws.ToTime(e.EventTime)}
	if !watchedSources[ev.Source] {
		return ev, false
	}
	var raw struct {
		UserIdentity struct {
			Type      string `json:"type"`
			ARN       string `json:"arn"`
			InvokedBy string `json:"invokedBy"`
		} `json:"userIdentity"`
		UserAgent         string `json:"userAgent"`
		ErrorCode         string `json:"errorCode"`
		RequestParameters any    `json:"requestParameters"`
		ResponseElements  any    `json:"responseElements"`
	}
	if err := json.Unmarshal([]byte(aws.ToString(e.CloudTrailEvent)), &raw); err != nil {
		return ev, false
	}
	if raw.ErrorCode != "" {
		return ev, false // the call failed: nothing changed
	}
	ev.Who = shortIdentity(raw.UserIdentity.ARN, raw.UserIdentity.Type, raw.UserIdentity.InvokedBy, aws.ToString(e.Username))
	ev.Agent = agentOf(raw.UserAgent)
	seen := map[string]bool{}
	add := func(s string) {
		if len(s) >= 4 && !seen[s] {
			seen[s] = true
			ev.Values = append(ev.Values, s)
		}
	}
	for _, r := range e.Resources {
		add(aws.ToString(r.ResourceName))
	}
	collect(raw.RequestParameters, add, 0)
	collect(raw.ResponseElements, add, 0)
	return ev, true
}

// collect walks decoded JSON and passes every string leaf to add. Policy
// documents and the like are long strings; they are passed whole, which
// matches nothing, rather than parsed.
func collect(v any, add func(string), depth int) {
	if depth > 8 {
		return
	}
	switch x := v.(type) {
	case string:
		if len(x) <= 2048 {
			add(x)
		}
	case []any:
		for _, e := range x {
			collect(e, add, depth+1)
		}
	case map[string]any:
		for _, e := range x {
			collect(e, add, depth+1)
		}
	}
}

// shortIdentity names the caller readably: user/alice, assumed-role/admin/bob,
// root, or the AWS service that acted on someone's behalf.
func shortIdentity(arn, typ, invokedBy, username string) string {
	switch {
	case invokedBy != "" && arn == "":
		return invokedBy
	case typ == "Root":
		return "root"
	case arn != "":
		if i := strings.LastIndex(arn, ":"); i >= 0 {
			return arn[i+1:]
		}
		return arn
	case username != "":
		return username
	}
	return "unknown identity"
}

// agentOf turns a user agent into the tool's name.
func agentOf(ua string) string {
	l := strings.ToLower(ua)
	switch {
	case ua == "":
		return ""
	case strings.Contains(l, "terraform"):
		return "Terraform"
	case strings.Contains(l, "console.amazonaws.com"), strings.Contains(l, "signin.amazonaws.com"), strings.HasPrefix(l, "aws internal"):
		return "console"
	case strings.Contains(l, "cloudformation"):
		return "CloudFormation"
	case strings.HasPrefix(l, "aws-cli"):
		return "aws-cli"
	case strings.Contains(l, "boto3"):
		return "boto3"
	case strings.HasSuffix(l, ".amazonaws.com"):
		return ua
	}
	if i := strings.IndexAny(ua, "/ "); i > 0 {
		return ua[:i]
	}
	return ua
}

// Touches reports whether the event names the resource with this import ID,
// or the resource it belongs to (a role, for its policies).
func (e WriteEvent) Touches(importID string) bool {
	for _, v := range e.Values {
		switch {
		case v == importID,
			strings.HasPrefix(importID, v+"/"), strings.HasPrefix(importID, v+":"), // owner: role/..., role:policy
			strings.HasSuffix(importID, "/"+v), // subnet/rtb-..., by the route table
			strings.HasPrefix(v, "arn:") && (strings.HasSuffix(v, "/"+importID) || strings.HasSuffix(v, ":"+importID)):
			return true
		}
	}
	return false
}
