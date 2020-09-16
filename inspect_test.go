package main

import (
	"testing"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/thanos-io/thanos/pkg/block/metadata"
)

func Test_parseFlagLabels(t *testing.T) {
	cases := []struct {
		s         []string
		expectErr bool
	}{
		{
			s:         []string{`labelName="LabelVal"`, `_label_Name="LabelVal"`, `label_name="LabelVal"`, `LAb_el_Name="LabelValue"`, `lab3l_Nam3=LabelValue `},
			expectErr: false,
		},
		{
			s:         []string{`label-Name="LabelVal"`}, // Unsupported labelname.
			expectErr: true,
		},
		{
			s:         []string{`label:Name="LabelVal"`}, // Unsupported labelname.
			expectErr: true,
		},
		{
			s:         []string{`1abelName="LabelVal"`}, // Unsupported labelname.
			expectErr: true,
		},
		{
			s:         []string{`label_Name"LabelVal"`}, // Missing "=" seprator.
			expectErr: true,
		},
		{
			s:         []string{`label_Name= "LabelVal"`}, // Whitespace invalid syntax.
			expectErr: true,
		},
	}
	for _, td := range cases {
		_, err := parseFlagLabels(td.s)
		if (err != nil) != td.expectErr {
			t.Errorf("parseFlagLabels(%q) err=%v, wants %v", td.s, err != nil, td.expectErr)
		}
	}
}

func Test_matchesSelector(t *testing.T) {
	cases := []struct {
		thanosLabels   map[string]string
		selectorLabels labels.Labels
		res            bool
	}{
		{
			thanosLabels: map[string]string{"label": "value"},
			selectorLabels: labels.Labels{
				{"label", "value"},
			},
			res: true,
		},
		{
			thanosLabels: map[string]string{
				"label":   "value",
				"label2":  "value2",
				"label3 ": "value3 ",
			},
			selectorLabels: labels.Labels{
				{"label", "value"},
				{"label3 ", "value3 "},
			},
			res: true,
		},
		{
			thanosLabels: map[string]string{
				"label":  "value",
				"label2": "value2",
			},
			selectorLabels: labels.Labels{
				{"label", "value"},
				{"label3 ", "value3 "},
			},
			res: false,
		},
	}
	for _, td := range cases {
		blockMeta := new(metadata.Meta)
		blockMeta.Thanos.Labels = td.thanosLabels
		res := matchesSelector(blockMeta, td.selectorLabels)
		if res != td.res {
			t.Errorf("matchesSelector(%q, %q)=%v, wants %v", td.thanosLabels, td.selectorLabels, res, td.res)
		}
	}
}
