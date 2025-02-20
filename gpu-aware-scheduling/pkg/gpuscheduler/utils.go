package gpuscheduler

import (
	"regexp"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// resourcePrefix is the intel resource prefix.
	resourcePrefix    = "gpu.intel.com/"
	pciGroupLabel     = "gpu.intel.com/pci-groups"
	regexCardTile     = "^card([0-9]+)_gt([0-9]+)$"
	digitBase         = 10
	desiredIntBits    = 16
	regexDesiredCount = 3
)

type DisabledTilesMap map[string][]int
type DescheduledTilesMap map[string][]int
type PreferredTilesMap map[string][]int

// Return all resources requests and samegpuSearchmap indicating which resourceRequests
// should be counted together. samegpuSearchmap is same length as samegpuContainerNames arg,
// Key is index of allResource item, value is true if container was listed in same-gpu annotation.
func containerRequests(pod *v1.Pod, samegpuContainerNames map[string]bool) (
	map[int]bool, []resourceMap) {
	samegpuSearchMap := map[int]bool{}
	allResources := []resourceMap{}

	for idx, container := range pod.Spec.Containers {
		rm := resourceMap{}

		for name, quantity := range container.Resources.Requests {
			resourceName := name.String()
			if strings.HasPrefix(resourceName, gpuPrefix) {
				value, _ := quantity.AsInt64()
				rm[resourceName] = value
			}
		}

		if samegpuContainerNames[container.Name] {
			samegpuSearchMap[idx] = true
		}

		allResources = append(allResources, rm)
	}

	return samegpuSearchMap, allResources
}

// addPCIGroupGPUs processes the given card and if it is requested to be handled as groups, the
// card's group is added to the cards slice.
func addPCIGroupGPUs(node *v1.Node, card string, cards []string) []string {
	pciGroupGPUNums := getPCIGroup(node, card)
	for _, gpuNum := range pciGroupGPUNums {
		groupedCard := "card" + gpuNum
		if found := containsString(cards, groupedCard); !found {
			cards = append(cards, groupedCard)
		}
	}

	return cards
}

func createTileMapping(labels map[string]string) (
	DisabledTilesMap, DescheduledTilesMap, PreferredTilesMap) {
	disabled := DisabledTilesMap{}
	descheduled := DescheduledTilesMap{}
	preferred := PreferredTilesMap{}

	extractCardAndTile := func(cardTileCombo string) (card string, tile int, err error) {
		card = ""
		tile = -1

		re := regexp.MustCompile(regexCardTile)

		values := re.FindStringSubmatch(cardTileCombo)
		if len(values) != regexDesiredCount {
			return card, tile, errExtractFail
		}

		card = "card" + values[1]
		tile, _ = strconv.Atoi(values[2])

		return card, tile, nil
	}

	for label, value := range labels {
		stripped, ok := labelWithoutTASNS(label)
		if !ok {
			continue
		}

		switch {
		case strings.HasPrefix(stripped, tileDisableLabelPrefix):
			{
				cardTileCombo := strings.TrimPrefix(stripped, tileDisableLabelPrefix)

				card, tile, err := extractCardAndTile(cardTileCombo)
				if err == nil {
					disabled[card] = append(disabled[card], tile)
				}
			}
		case strings.HasPrefix(stripped, tileDeschedLabelPrefix):
			{
				cardTileCombo := strings.TrimPrefix(stripped, tileDeschedLabelPrefix)

				card, tile, err := extractCardAndTile(cardTileCombo)
				if err == nil {
					descheduled[card] = append(descheduled[card], tile)
				}
			}
		case strings.HasPrefix(stripped, tilePrefLabelPrefix):
			{
				cardWithoutTile := strings.TrimPrefix(stripped, tilePrefLabelPrefix)
				cardWithTile := cardWithoutTile + "_" + value

				card, tile, err := extractCardAndTile(cardWithTile)
				if err == nil {
					preferred[card] = append(preferred[card], tile)
				}
			}
		default:
			continue
		}
	}

	return disabled, descheduled, preferred
}

func combineMappings(source map[string][]int, dest map[string][]int) {
	for card, tiles := range source {
		dest[card] = append(dest[card], tiles...)
	}
}

// creates a card to tile-index map which are in either state "disabled" or "descheduled".
func createDisabledTileMapping(labels map[string]string) map[string][]int {
	dis, des, _ := createTileMapping(labels)

	combineMappings(des, dis)

	return dis
}

// creates two card to tile-index maps where first is disabled and second is preferred mapping.
func createDisabledAndPreferredTileMapping(labels map[string]string) (
	DisabledTilesMap, PreferredTilesMap) {
	dis, des, pref := createTileMapping(labels)

	combineMappings(des, dis)

	return dis, pref
}

func sanitizeTiles(tilesMap DisabledTilesMap, tilesPerGpu int) DisabledTilesMap {
	sanitized := DisabledTilesMap{}

	for card, tiles := range tilesMap {
		stiles := []int{}

		for _, tile := range tiles {
			if tile < tilesPerGpu {
				stiles = append(stiles, tile)
			} else {
				klog.Warningf("skipping a non existing tile: %s, tile %d", card, tile)
			}
		}

		sanitized[card] = stiles
	}

	return sanitized
}

func labelWithoutTASNS(label string) (string, bool) {
	if strings.HasPrefix(label, tasNSPrefix) {
		parts := strings.Split(label, "/")
		if len(parts) == maxLabelParts {
			return parts[1], true
		}
	}

	return "", false
}

func isGPUInPCIGroup(gpuName, pciGroupGPUName string, node *v1.Node) bool {
	gpuNums := getPCIGroup(node, pciGroupGPUName)
	for _, gpuNum := range gpuNums {
		if gpuName == "card"+gpuNum {
			return true
		}
	}

	return false
}

// concatenateSplitLabel returns the given label value and concatenates any
// additional values for label names with a running number postfix starting with "2".
func concatenateSplitLabel(node *v1.Node, labelName string) string {
	postFix := 2
	value := node.Labels[labelName]

	for continuingLabelValue, ok := node.Labels[labelName+strconv.Itoa(postFix)]; ok; {
		value += continuingLabelValue
		postFix++
		continuingLabelValue, ok = node.Labels[labelName+strconv.Itoa(postFix)]
	}

	return value
}

// getPCIGroup returns the pci group as slice, for the given gpu name.
func getPCIGroup(node *v1.Node, gpuName string) []string {
	if pciGroups := concatenateSplitLabel(node, pciGroupLabel); pciGroups != "" {
		slicedGroups := strings.Split(pciGroups, "_")
		for _, group := range slicedGroups {
			gpuNums := strings.Split(group, ".")
			for _, gpuNum := range gpuNums {
				if "card"+gpuNum == gpuName {
					return gpuNums
				}
			}
		}
	}

	return []string{}
}

func hasGPUCapacity(node *v1.Node) bool {
	if node == nil {
		return false
	}

	if quantity, ok := node.Status.Capacity[gpuPluginResource]; ok {
		numI915, _ := quantity.AsInt64()
		if numI915 > 0 {
			return true
		}
	}

	return false
}

func hasGPUResources(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}

	for i := 0; i < len(pod.Spec.Containers); i++ {
		container := &pod.Spec.Containers[i]
		for name := range container.Resources.Requests {
			resourceName := name.String()
			if strings.HasPrefix(resourceName, resourcePrefix) {
				return true
			}
		}
	}

	return false
}

func isCompletedPod(pod *v1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return true
	}

	switch pod.Status.Phase {
	case v1.PodFailed:
		fallthrough
	case v1.PodSucceeded:
		return true
	case v1.PodPending:
		fallthrough
	case v1.PodRunning:
		fallthrough
	case v1.PodUnknown:
		fallthrough
	default:
		return false
	}
}

func containsInt(slice []int, value int) (bool, int) {
	for index, v := range slice {
		if v == value {
			return true, index
		}
	}

	return false, -1
}

func containsString(slice []string, value string) bool {
	for _, v := range slice {
		if v == value {
			return true
		}
	}

	return false
}

func reorderPreferredTilesFirst(tiles []int, preferred []int) []int {
	indexNow := 0

	for _, pref := range preferred {
		if found, index := containsInt(tiles, pref); found {
			if index > indexNow {
				old := tiles[indexNow]
				tiles[indexNow] = pref
				tiles[index] = old
			}

			indexNow++
		}
	}

	return tiles
}

func convertPodTileAnnotationToCardTileMap(podTileAnnotation string) map[string]bool {
	cardTileIndices := make(map[string]bool)

	containerCardList := strings.Split(podTileAnnotation, "|")

	for _, contAnnotation := range containerCardList {
		cardTileList := strings.Split(contAnnotation, ",")

		for _, cardTileCombos := range cardTileList {
			cardTileSplit := strings.Split(cardTileCombos, ":")
			if len(cardTileSplit) != maxLabelParts {
				continue
			}

			// extract card index by moving forward in slice
			cardIndexStr := cardTileSplit[0][len("card"):]

			_, err := strconv.ParseInt(cardIndexStr, digitBase, desiredIntBits)
			if err != nil {
				continue
			}

			tiles := strings.Split(cardTileSplit[1], "+")
			for _, tile := range tiles {
				tileNoStr := strings.TrimPrefix(tile, "gt")

				_, err := strconv.ParseInt(tileNoStr, digitBase, desiredIntBits)
				if err == nil {
					cardTileIndices[cardIndexStr+"."+tileNoStr] = true
				}
			}
		}
	}

	return cardTileIndices
}
