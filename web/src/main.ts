import { createApp } from 'vue'
import App from './App.vue'
import { installAssetRecovery } from './assetRecovery'
import { vTooltip } from './tooltip'
import './style.css'

installAssetRecovery()
createApp(App).directive('tooltip', vTooltip).mount('#app')
