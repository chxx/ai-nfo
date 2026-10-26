package main

type episodeNumber struct {
	ID      string `json:"id"`
	Season  *int   `json:"season"`
	Episode *int   `json:"episode"`
}

type episodeNumberInput struct {
	episodeNumber
	Path string `json:"path"`
}

func validateEpisodeNumbers(entries []episodeNumberInput, response []episodeNumber) (map[string]episodeNumber, error) {
	invalid := userError("AI 分集编号无效：必须完整返回原始 ID、保留已有编号，并使用 0 到 9999 的整数或 null，请重试或手动填写")
	if len(entries) != len(response) {
		return nil, invalid
	}
	expected := map[string]episodeNumberInput{}
	for _, entry := range entries {
		expected[entry.ID] = entry
	}
	numbers := map[string]episodeNumber{}
	occupied := map[[2]int]bool{}
	for _, number := range response {
		entry, exists := expected[number.ID]
		if _, duplicate := numbers[number.ID]; !exists || duplicate {
			return nil, invalid
		}
		for i, value := range []*int{number.Season, number.Episode} {
			original := []*int{entry.Season, entry.Episode}[i]
			if value != nil && (*value < 0 || *value > 9999) || original != nil && (value == nil || *value != *original) {
				return nil, invalid
			}
		}
		numbers[number.ID] = number
		if number.Season != nil && number.Episode != nil {
			key := [2]int{*number.Season, *number.Episode}
			changed := entry.Season == nil || entry.Episode == nil
			if previousChanged, exists := occupied[key]; exists && (changed || previousChanged) {
				return nil, userError("AI 返回的季数和集数存在重复，请重试或手动调整分集信息")
			}
			occupied[key] = changed
		}
	}
	return numbers, nil
}
