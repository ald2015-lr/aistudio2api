<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { init, use, type ECharts, type EChartsCoreOption } from 'echarts/core'
import { CanvasRenderer } from 'echarts/renderers'
import { BarChart, LineChart } from 'echarts/charts'
import {
  AriaComponent,
  GridComponent,
  LegendComponent,
  MarkLineComponent,
  TooltipComponent,
} from 'echarts/components'

// 只注册用到的图表与组件：ECharts 按需引入，整个用量页随页面懒加载，不进入主包
use([
  CanvasRenderer,
  BarChart,
  LineChart,
  AriaComponent,
  GridComponent,
  LegendComponent,
  MarkLineComponent,
  TooltipComponent,
])

const props = defineProps<{ option: EChartsCoreOption }>()

const element = ref<HTMLDivElement>()
const chart = shallowRef<ECharts>()
let observer: ResizeObserver | undefined

onMounted(() => {
  if (element.value === undefined) return
  chart.value = init(element.value, undefined, { renderer: 'canvas' })
  chart.value.setOption(props.option, { notMerge: true })
  // 容器尺寸变化（侧栏收起、窗口缩放）时重新布局
  observer = new ResizeObserver(() => chart.value?.resize())
  observer.observe(element.value)
})

watch(
  () => props.option,
  (option) => chart.value?.setOption(option, { notMerge: true }),
)

onBeforeUnmount(() => {
  observer?.disconnect()
  chart.value?.dispose()
})
</script>

<template>
  <div ref="element" class="h-full w-full"></div>
</template>
