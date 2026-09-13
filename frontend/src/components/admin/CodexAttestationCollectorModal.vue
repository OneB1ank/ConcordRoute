<template>
  <BaseDialog
    :show="show"
    :title="t('admin.codexAttestationCollector.title')"
    width="wide"
    @close="$emit('close')"
  >
    <CodexAttestationCollectorCard :show="show" />
  </BaseDialog>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import CodexAttestationCollectorCard from './CodexAttestationCollectorCard.vue'

defineProps<{ show: boolean }>()
defineEmits<{ close: [] }>()

const { t } = useI18n()
</script>
