<template>
  <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
    <div class="min-w-0">
      <label :for="`${testId}-input`" class="input-label mb-0">
        {{ t('admin.accounts.openai.codexTurnMode') }}
      </label>
      <p :id="`${testId}-hint`" class="mt-1 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.openai.codexTurnModeDesc') }}
      </p>
    </div>
    <Select
      :id="`${testId}-input`"
      :model-value="modelValue"
      :options="options"
      :disabled="disabled"
      :aria-describedby="`${testId}-hint`"
      :data-testid="testId"
      class="w-full flex-shrink-0 sm:w-64"
      @update:model-value="emit('update:modelValue', $event === 'converge' ? 'converge' : 'passthrough')"
    />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import type { CodexTurnMode } from '@/utils/codexTurnMode'

defineProps<{ modelValue: CodexTurnMode; testId: string; disabled?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: CodexTurnMode] }>()
const { t } = useI18n()

// 创建、编辑、批量编辑和导入默认值共用同一组选择与说明。
const options = computed(() => [
  { value: 'passthrough', label: t('admin.accounts.openai.codexTurnPassthrough') },
  { value: 'converge', label: t('admin.accounts.openai.codexTurnConverge') }
])
</script>
