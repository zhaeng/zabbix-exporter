package metadata

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zhaeng/zabbix-exporter/internal/config"
	"github.com/zhaeng/zabbix-exporter/internal/zabbix"

	"github.com/prometheus/prometheus/prompb"
)

var (
	invalidMetricCharPattern = regexp.MustCompile(`[^a-zA-Z0-9_]+`)
	bracketContentPattern    = regexp.MustCompile(`\[([^\]]+)\]`)
	snmpInterfacePattern     = regexp.MustCompile(`^if[A-Z][A-Za-z0-9_]*\.(\d+)$`)
	labelTemplatePattern     = regexp.MustCompile(`\{\{\s*([^{}\s]+)\s*\}\}`)
)

// BuildStats exposes aggregated fallback information without attaching
// high-cardinality item IDs to logs or metrics.
type BuildStats struct {
	DelayFallbacks map[DelayParseStatus]int
}

// Builder converts complete Zabbix host/item responses into a validated,
// immutable snapshot candidate.
type Builder struct {
	labels        config.LabelsConfig
	delayFallback time.Duration
}

func NewBuilder(labels config.LabelsConfig, delayFallback time.Duration) *Builder {
	if delayFallback <= 0 {
		delayFallback = time.Minute
	}
	return &Builder{labels: cloneLabelConfig(labels), delayFallback: delayFallback}
}

func (b *Builder) Build(hosts []zabbix.Host, items []zabbix.Item, createdAt time.Time) (*MetadataSnapshot, BuildStats, error) {
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	stats := BuildStats{DelayFallbacks: make(map[DelayParseStatus]int)}
	snapshot := &MetadataSnapshot{
		CreatedAt:  createdAt,
		Hosts:      make(map[string]*HostMetadata, len(hosts)),
		Items:      make(map[string]*ItemMetadata, len(items)),
		HostItems:  make(map[string][]string, len(hosts)),
		Groups:     make(map[string]*CollectionGroup),
		ItemGroups: make(map[string]string, len(items)),
	}

	for i := range hosts {
		host := &hosts[i]
		if host.HostID == "" {
			return nil, stats, fmt.Errorf("host at index %d has an empty hostid", i)
		}
		if _, exists := snapshot.Hosts[host.HostID]; exists {
			return nil, stats, fmt.Errorf("duplicate hostid %q", host.HostID)
		}
		inventoryFields := extractInventoryFields(host.Inventory)
		hostIP := selectHostIP(host.Interfaces)
		hostName := host.Host
		if hostName == "" {
			hostName = host.Name
		}
		metadata := &HostMetadata{
			HostID:          host.HostID,
			HostName:        hostName,
			DisplayName:     host.Name,
			HostIP:          hostIP,
			Inventory:       cloneInventory(host.ParseInventory()),
			InventoryFields: inventoryFields,
			RefreshedAt:     createdAt,
		}
		metadata.Labels = b.buildHostLabels(metadata)
		snapshot.Hosts[host.HostID] = metadata
		snapshot.HostItems[host.HostID] = []string{}
	}

	for i := range items {
		item := &items[i]
		if item.ItemID == "" {
			return nil, stats, fmt.Errorf("item at index %d has an empty itemid", i)
		}
		if _, exists := snapshot.Items[item.ItemID]; exists {
			return nil, stats, fmt.Errorf("duplicate itemid %q", item.ItemID)
		}
		host, exists := snapshot.Hosts[item.HostID]
		if !exists {
			return nil, stats, fmt.Errorf("item %q references unknown host %q", item.ItemID, item.HostID)
		}
		if item.ValueType != "0" && item.ValueType != "3" {
			return nil, stats, fmt.Errorf("item %q has unsupported value_type %q", item.ItemID, item.ValueType)
		}

		delay, parseState := ParseDelay(item.Delay, b.delayFallback)
		if parseState != DelayParsed {
			stats.DelayFallbacks[parseState]++
		}
		metadata := &ItemMetadata{
			ItemID:          item.ItemID,
			HostID:          item.HostID,
			Key:             item.Key,
			Name:            item.Name,
			ValueType:       item.ValueType,
			RawDelay:        item.Delay,
			Delay:           delay,
			DelayParseState: parseState,
			Status:          item.Status,
			State:           item.State,
			Enabled:         statusEnabled(item.Status) && statusEnabled(item.State),
			RefreshedAt:     createdAt,
		}
		metadata.MetricName, metadata.Labels = b.buildItemLabels(host, metadata)
		snapshot.Items[item.ItemID] = metadata
		snapshot.HostItems[item.HostID] = append(snapshot.HostItems[item.HostID], item.ItemID)
	}
	snapshot.HostIDs = sortedHostIDs(snapshot.Hosts)

	for _, itemIDs := range snapshot.HostItems {
		sort.Strings(itemIDs)
	}
	for _, item := range snapshot.Items {
		if !item.Enabled {
			continue
		}
		groupID := collectionGroupID(item.HostID, item.ValueType, item.Delay)
		group := snapshot.Groups[groupID]
		if group == nil {
			group = &CollectionGroup{
				ID:        groupID,
				HostID:    item.HostID,
				ValueType: item.ValueType,
				Delay:     item.Delay,
			}
			snapshot.Groups[groupID] = group
		}
		group.ItemIDs = append(group.ItemIDs, item.ItemID)
		snapshot.ItemGroups[item.ItemID] = groupID
	}
	for _, group := range snapshot.Groups {
		sort.Strings(group.ItemIDs)
	}

	return snapshot, stats, nil
}

func statusEnabled(value string) bool {
	return value == "" || value == "0"
}

func collectionGroupID(hostID, valueType string, delay time.Duration) string {
	return hostID + "/" + valueType + "/" + strconv.FormatInt(int64(delay/time.Second), 10)
}

func selectHostIP(interfaces []zabbix.Interface) string {
	for _, iface := range interfaces {
		if iface.Main == "1" && iface.Type == "1" && iface.IP != "" {
			return iface.IP
		}
	}
	for _, iface := range interfaces {
		if iface.Main == "1" && iface.IP != "" {
			return iface.IP
		}
	}
	for _, iface := range interfaces {
		if iface.Type == "1" && iface.IP != "" {
			return iface.IP
		}
	}
	for _, iface := range interfaces {
		if iface.IP != "" {
			return iface.IP
		}
	}
	return ""
}

func (b *Builder) buildHostLabels(host *HostMetadata) []prompb.Label {
	labels := map[string]string{
		"__name__": "zabbix_host",
		"host":     host.HostName,
	}
	if host.HostIP != "" {
		labels["host_ip"] = host.HostIP
		labels["vm_id"] = host.HostIP
	}
	b.mergeConfiguredLabels(labels, b.labels.Global, host)
	b.mergeConfiguredLabels(labels, b.labels.Host, host)
	return canonicalLabels(labels)
}

func (b *Builder) buildItemLabels(host *HostMetadata, item *ItemMetadata) (string, []prompb.Label) {
	metricName := normalizeMetricName(item.Key)
	labels := map[string]string{
		"__name__":  metricName,
		"host":      host.HostName,
		"item_key":  item.Key,
		"item_name": item.Name,
	}
	if isParameterizedNetworkMetric(item.Key) {
		metricName = normalizeParameterizedName(item.Key)
		labels["__name__"] = metricName
		if isNetworkInterfaceMetric(item.Key) {
			if iface := parseNetworkInterface(item.Key); iface != "" {
				labels["interface"] = iface
			}
		}
	} else if strings.Contains(item.Key, "[") && strings.Contains(item.Key, "]") {
		metricName = normalizeParameterizedName(item.Key)
		labels["__name__"] = metricName
		if match := bracketContentPattern.FindStringSubmatch(item.Key); len(match) > 1 {
			labels["sensor"] = match[1]
		}
	}
	if host.HostIP != "" {
		labels["host_ip"] = host.HostIP
		labels["vm_id"] = host.HostIP
	}
	b.mergeConfiguredLabels(labels, b.labels.Global, host)
	b.mergeConfiguredLabels(labels, b.labels.Host, host)
	b.mergeConfiguredLabels(labels, b.labels.Item, host)
	return metricName, canonicalLabels(labels)
}

func (b *Builder) mergeConfiguredLabels(dst, configured map[string]string, host *HostMetadata) {
	for name, value := range configured {
		dst[name] = resolveLabelTemplate(value, host)
	}
}

func resolveLabelTemplate(value string, host *HostMetadata) string {
	return labelTemplatePattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := labelTemplatePattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return ""
		}
		key := strings.ToLower(parts[1])
		switch key {
		case "host.id", "host.hostid":
			return host.HostID
		case "host.host", "host.hostname":
			return host.HostName
		case "host.name":
			return host.DisplayName
		case "host.ip", "host.hostip":
			return host.HostIP
		}
		for _, prefix := range []string{"host.inventory.", "host.metadata.", "inventory."} {
			if strings.HasPrefix(key, prefix) {
				return host.InventoryFields[strings.TrimPrefix(key, prefix)]
			}
		}
		return ""
	})
}

func canonicalLabels(values map[string]string) []prompb.Label {
	labels := make([]prompb.Label, 0, len(values))
	for name, value := range values {
		labels = append(labels, prompb.Label{Name: name, Value: value})
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	return labels
}

func normalizeMetricName(key string) string {
	name := invalidMetricCharPattern.ReplaceAllString(key, "_")
	if len(name) > 0 && name[0] >= '0' && name[0] <= '9' {
		return "zabbix_" + name
	}
	return name
}

func normalizeParameterizedName(key string) string {
	match := bracketContentPattern.FindStringIndex(key)
	if len(match) == 0 {
		return normalizeMetricName(key)
	}
	return normalizeMetricName(strings.TrimSuffix(key[:match[0]], "."))
}

func isParameterizedNetworkMetric(key string) bool {
	return isNetworkInterfaceMetric(key) || strings.HasPrefix(key, "net.tcp.") || strings.HasPrefix(key, "net.udp.")
}

func isNetworkInterfaceMetric(key string) bool {
	return strings.HasPrefix(key, "net.if.")
}

func parseNetworkInterface(itemKey string) string {
	match := bracketContentPattern.FindStringSubmatch(itemKey)
	if len(match) < 2 {
		return ""
	}
	value := strings.TrimSpace(strings.SplitN(match[1], ",", 2)[0])
	if match := snmpInterfacePattern.FindStringSubmatch(value); len(match) > 1 {
		return match[1]
	}
	return value
}

func extractInventoryFields(value any) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return map[string]string{}
		}
		var decoded any
		if err := json.Unmarshal([]byte(typed), &decoded); err != nil {
			return map[string]string{}
		}
		return extractInventoryFields(decoded)
	case []any:
		if len(typed) == 0 {
			return map[string]string{}
		}
		return extractInventoryFields(typed[0])
	}
	data, err := json.Marshal(value)
	if err != nil {
		return map[string]string{}
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return map[string]string{}
	}
	result := make(map[string]string, len(fields))
	for key, field := range fields {
		key = strings.ToLower(key)
		switch typed := field.(type) {
		case string:
			result[key] = typed
		case float64:
			result[key] = strconv.FormatFloat(typed, 'f', -1, 64)
		case bool:
			result[key] = strconv.FormatBool(typed)
		}
	}
	return result
}

func cloneInventory(inventory *zabbix.HostInventory) *zabbix.HostInventory {
	if inventory == nil {
		return nil
	}
	clone := *inventory
	return &clone
}

func cloneLabelConfig(labels config.LabelsConfig) config.LabelsConfig {
	return config.LabelsConfig{
		Global: cloneStringMap(labels.Global),
		Host:   cloneStringMap(labels.Host),
		Item:   cloneStringMap(labels.Item),
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
