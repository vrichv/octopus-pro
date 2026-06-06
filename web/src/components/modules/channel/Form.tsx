import { AutoGroupType, ChannelType, type Channel, useFetchModel } from '@/api/endpoints/channel';
import {
    Select,
    SelectContent,
    SelectItem,
    SelectTrigger,
    SelectValue,
} from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { toast } from '@/components/common/Toast';
import { useTranslations } from 'next-intl';
import { useEffect, useMemo, useRef, useState } from 'react';
import { RefreshCw, X, Plus } from 'lucide-react';

export interface ChannelKeyFormItem {
    id?: number;
    enabled: boolean;
    channel_key: string;
    status_code?: number;
    last_use_time_stamp?: number;
    total_cost?: number;
    remark?: string;
    key_proxy?: string;
}

export interface ChannelFormData {
    name: string;
    type: ChannelType;
    base_urls: Channel['base_urls'];
    custom_header: Channel['custom_header'];
    channel_proxy: string;
    param_override: string;
    keys: ChannelKeyFormItem[];
    model: string;
    custom_model: string;
    excluded_model: string;
    enabled: boolean;
    proxy: boolean;
    auto_sync: boolean;
    auto_group: AutoGroupType;
    match_regex: string;
    rate_limit: string;
    model_rate_limit: string;
    key_mode: number;
}

export interface ChannelFormProps {
    formData: ChannelFormData;
    onFormDataChange: (data: ChannelFormData) => void;
    onSubmit: (event: React.FormEvent<HTMLFormElement>) => void;
    isPending: boolean;
    submitText: string;
    pendingText: string;
    onCancel?: () => void;
    cancelText?: string;
    idPrefix?: string;
}

import {
    Accordion,
    AccordionContent,
    AccordionItem,
    AccordionTrigger,
} from "@/components/ui/accordion";

const splitModels = (models: string) => models
    ? models.split(',').map((model) => model.trim()).filter(Boolean)
    : [];

const dedupeModels = (models: string[]) => {
    const seen = new Set<string>();
    const nextModels: string[] = [];

    for (const model of models) {
        const trimmedModel = model.trim();
        if (!trimmedModel || seen.has(trimmedModel)) continue;
        seen.add(trimmedModel);
        nextModels.push(trimmedModel);
    }

    return nextModels;
};

export function ChannelForm({
    formData,
    onFormDataChange,
    onSubmit,
    isPending,
    submitText,
    pendingText,
    onCancel,
    cancelText,
    idPrefix = 'channel',
}: ChannelFormProps) {
    const t = useTranslations('channel.form');

    // Ensure the form always shows at least 1 row for base_urls / keys / custom_header.
    // This avoids "empty list" UI and also keeps URL + APIKEY layout consistent.
    useEffect(() => {
        if (!formData.base_urls || formData.base_urls.length === 0) {
            onFormDataChange({ ...formData, base_urls: [{ url: '', delay: 0 }] });
            return;
        }
        if (!formData.keys || formData.keys.length === 0) {
            onFormDataChange({ ...formData, keys: [{ enabled: true, channel_key: '' }] });
            return;
        }
        if (!formData.custom_header || formData.custom_header.length === 0) {
            onFormDataChange({ ...formData, custom_header: [{ header_key: '', header_value: '' }] });
        }
    }, [formData, onFormDataChange]);

    const autoModels = useMemo(() => dedupeModels(splitModels(formData.model)), [formData.model]);
    const customModels = useMemo(() => dedupeModels(splitModels(formData.custom_model)), [formData.custom_model]);
    const excludedModels = useMemo(() => dedupeModels(splitModels(formData.excluded_model)), [formData.excluded_model]);
    const selectedModels = useMemo(() => dedupeModels([...autoModels, ...customModels]), [autoModels, customModels]);
    const selectedModelSet = useMemo(() => new Set(selectedModels), [selectedModels]);
    const selectedAutoModelRef = useRef<string[]>(autoModels);
    const selectedCustomModelRef = useRef<string[]>(customModels);
    const disabledAutoModelRef = useRef<string[]>(excludedModels);
    const availableModelsRef = useRef<string[]>(dedupeModels([...autoModels, ...excludedModels]));
    const [inputValue, setInputValue] = useState('');
    const [modelSearch, setModelSearch] = useState('');
    const [availableModels, setAvailableModels] = useState<string[]>(() => dedupeModels([...autoModels, ...excludedModels]));
    const [availableCustomModels, setAvailableCustomModels] = useState<string[]>(() => customModels);
    const inputRef = useRef<HTMLInputElement>(null);

    const fetchModel = useFetchModel();
    const hasAutoFetchedRef = useRef(false);

    const effectiveKey =
        formData.keys.find((k) => k.enabled && k.channel_key.trim())?.channel_key.trim() || '';

    const updateModels = (nextAuto: string[], nextCustom: string[], nextExcluded: string[]) => {
        const model = dedupeModels(nextAuto).join(',');
        const custom_model = dedupeModels(nextCustom).join(',');
        const nextExcludedModels = dedupeModels(nextExcluded);
        const excluded_model = nextExcludedModels.join(',');
        selectedAutoModelRef.current = dedupeModels(nextAuto);
        selectedCustomModelRef.current = dedupeModels(nextCustom);
        disabledAutoModelRef.current = nextExcludedModels;
        if (formData.model === model && formData.custom_model === custom_model && formData.excluded_model === excluded_model) return;
        onFormDataChange({ ...formData, model, custom_model, excluded_model });
    };

    // Auto-fetch full model list on initial load when editing an existing channel.
    // This ensures excluded models are visible as gray before any manual refresh.
    useEffect(() => {
        if (hasAutoFetchedRef.current) return;
        if (!formData.base_urls?.[0]?.url || !effectiveKey) return;
        if (!formData.model && !formData.excluded_model) return;
        hasAutoFetchedRef.current = true;
        fetchModel.mutate(
            {
                type: formData.type,
                base_urls: formData.base_urls,
                keys: formData.keys
                    .filter((k) => k.channel_key.trim())
                    .map((k) => ({ enabled: k.enabled, channel_key: k.channel_key.trim() })),
                proxy: formData.proxy,
                channel_proxy: formData.channel_proxy?.trim() || null,
                match_regex: formData.match_regex.trim() || null,
                custom_header: formData.custom_header?.filter((h) => h.header_key.trim()) || [],
            },
            {
                onSuccess: (data) => {
                    const fetchedModels = dedupeModels(data ?? []);
                    if (fetchedModels.length === 0) return;
                    const fetchedModelSet = new Set(fetchedModels);
                    const currentAutoModelSet = new Set(selectedAutoModelRef.current);
                    const currentCustomModelSet = new Set(selectedCustomModelRef.current);
                    // Models not in autoModels or customModels are excluded (gray)
                    const nextExcludedModels = dedupeModels([
                        ...disabledAutoModelRef.current,
                        ...fetchedModels.filter((model) =>
                            !currentAutoModelSet.has(model) && !currentCustomModelSet.has(model)
                        ),
                    ]).filter((model) => fetchedModelSet.has(model));
                    const nextExcludedModelSet = new Set(nextExcludedModels);
                    const nextAutoModels = fetchedModels.filter((model) => !nextExcludedModelSet.has(model));
                    availableModelsRef.current = fetchedModels;
                    setAvailableModels(fetchedModels);
                    updateModels(nextAutoModels, selectedCustomModelRef.current, nextExcludedModels);
                },
            }
        );
    }, [formData.base_urls, effectiveKey, formData.model, formData.excluded_model]);

    // Keep refs in sync with formData changes so handleRefreshModels always has up-to-date data
    useEffect(() => {
        selectedAutoModelRef.current = autoModels;
        selectedCustomModelRef.current = customModels;
        disabledAutoModelRef.current = excludedModels;
        availableModelsRef.current = dedupeModels([...autoModels, ...excludedModels]);
    }, [autoModels, customModels, excludedModels]);


    const allAvailableModels = useMemo(() => (
        dedupeModels([...availableModels, ...availableCustomModels, ...autoModels, ...customModels, ...excludedModels])
    ), [autoModels, availableCustomModels, availableModels, customModels, excludedModels]);

    const visibleModels = useMemo(() => {
        const searchTerm = modelSearch.trim().toLowerCase();
        if (!searchTerm) return allAvailableModels;
        return allAvailableModels.filter((model) => model.toLowerCase().includes(searchTerm));
    }, [allAvailableModels, modelSearch]);

    const displayModels = useMemo(() => (
        [...visibleModels].sort((left, right) => Number(selectedModelSet.has(right)) - Number(selectedModelSet.has(left)))
    ), [selectedModelSet, visibleModels]);

    const selectedVisibleModelCount = visibleModels.filter((model) => selectedModelSet.has(model)).length;
    const allVisibleModelsSelected = visibleModels.length > 0 && selectedVisibleModelCount === visibleModels.length;

    const selectModels = (models: string[]) => {
        const nextModels = dedupeModels(models);
        if (nextModels.length === 0) return;

        const availableCustomModelSet = new Set(availableCustomModels);
        const currentAutoModels = selectedAutoModelRef.current;
        const currentCustomModels = selectedCustomModelRef.current;
        const nextAutoModels = [...currentAutoModels];
        const nextCustomModels = [...currentCustomModels];
        const selectedModelNames = new Set([...currentAutoModels, ...currentCustomModels]);

        for (const model of nextModels) {
            if (selectedModelNames.has(model)) continue;
            if (availableCustomModelSet.has(model)) {
                nextCustomModels.push(model);
            } else {
                nextAutoModels.push(model);
            }
            selectedModelNames.add(model);
        }

        const selectedAutoModels = nextModels.filter((model) => !availableCustomModelSet.has(model));
        setAvailableModels((currentModels) => {
            const next = dedupeModels([...currentModels, ...selectedAutoModels]);
            availableModelsRef.current = next;
            return next;
        });
        setAvailableCustomModels((currentModels) => currentModels.filter((model) => !nextModels.includes(model)));
        updateModels(nextAutoModels, nextCustomModels, disabledAutoModelRef.current.filter((model) => !nextModels.includes(model)));
    };

    const deselectModels = (models: string[]) => {
        const modelsToDeselect = new Set(models);
        if (modelsToDeselect.size === 0) return;

        const currentAutoModels = selectedAutoModelRef.current;
        const currentCustomModels = selectedCustomModelRef.current;
        const deselectedAutoModels = currentAutoModels.filter((model) => modelsToDeselect.has(model));
        const deselectedCustomModels = currentCustomModels.filter((model) => modelsToDeselect.has(model));

        setAvailableModels((currentModels) => {
            const next = dedupeModels([...currentModels, ...deselectedAutoModels]);
            availableModelsRef.current = next;
            return next;
        });
        setAvailableCustomModels((currentModels) => dedupeModels([...currentModels, ...deselectedCustomModels]));
        updateModels(
            currentAutoModels.filter((model) => !modelsToDeselect.has(model)),
            currentCustomModels.filter((model) => !modelsToDeselect.has(model)),
            dedupeModels([...disabledAutoModelRef.current, ...deselectedAutoModels])
        );
    };

    const toggleModel = (model: string) => {
        if (selectedModelSet.has(model)) {
            deselectModels([model]);
        } else {
            selectModels([model]);
        }
    };

    const handleRefreshModels = async () => {
        if (!formData.base_urls?.[0]?.url || !effectiveKey) return;
        fetchModel.mutate(
            {
                type: formData.type,
                base_urls: formData.base_urls,
                keys: formData.keys
                    .filter((k) => k.channel_key.trim())
                    .map((k) => ({ enabled: k.enabled, channel_key: k.channel_key.trim() })),
                proxy: formData.proxy,
                channel_proxy: formData.channel_proxy?.trim() || null,
                match_regex: formData.match_regex.trim() || null,
                custom_header: formData.custom_header?.filter((h) => h.header_key.trim()) || [],
            },
            {
                onSuccess: (data) => {
                    const fetchedModels = dedupeModels(data ?? []);
                    if (fetchedModels.length === 0) {
                        toast.warning(t('modelRefreshEmpty'));
                        return;
                    }
                    const fetchedModelSet = new Set(fetchedModels);
                    const currentAutoModels = selectedAutoModelRef.current;
                    const currentCustomModels = selectedCustomModelRef.current;
                    const currentExcludedModels = disabledAutoModelRef.current;
                    const currentAutoModelSet = new Set(currentAutoModels);
                    const currentCustomModelSet = new Set(currentCustomModels);
                    const currentExcludedModelSet = new Set(currentExcludedModels);
                    // New models from remote (not in selected, not in excluded) → add to excluded
                    const newRemoteModels = fetchedModels.filter((model) =>
                        !currentAutoModelSet.has(model) &&
                        !currentCustomModelSet.has(model) &&
                        !currentExcludedModelSet.has(model)
                    );
                    const nextExcludedModels = dedupeModels([...currentExcludedModels, ...newRemoteModels])
                        .filter((model) => fetchedModelSet.has(model));
                    const nextExcludedModelSet = new Set(nextExcludedModels);
                    const nextAutoModels = fetchedModels.filter((model) => !nextExcludedModelSet.has(model));
                    availableModelsRef.current = fetchedModels;
                    setAvailableModels(fetchedModels);
                    updateModels(nextAutoModels, currentCustomModels, nextExcludedModels);
                    toast.success(t('modelRefreshSuccess'));
                },
                onError: (error) => {
                    const errorMessage = error instanceof Error ? error.message : String(error);
                    toast.error(t('modelRefreshFailed'), { description: errorMessage });
                },
            }
        );
    };
    const handleAddModel = (model: string) => {
        const trimmedModel = model.trim();
        if (trimmedModel) {
            setAvailableCustomModels((currentModels) => dedupeModels([...currentModels, trimmedModel]));
            if (!selectedModelSet.has(trimmedModel)) {
                updateModels(selectedAutoModelRef.current, [...selectedCustomModelRef.current, trimmedModel], disabledAutoModelRef.current.filter((model) => model !== trimmedModel));
            }
        }
        setInputValue('');
    };

    const handleInputKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
        if (e.key === 'Enter') {
            e.preventDefault();
            if (inputValue.trim()) handleAddModel(inputValue);
        }
    };

    const handleModelSearchKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
        if (e.key === 'Enter') {
            e.preventDefault();
        }
    };

    const handleAddKey = () => {
        onFormDataChange({
            ...formData,
            keys: [...formData.keys, { enabled: true, channel_key: '' }],
        });
    };

    const handleUpdateKey = (idx: number, patch: Partial<ChannelKeyFormItem>) => {
        const next = formData.keys.map((k, i) => (i === idx ? { ...k, ...patch } : k));
        onFormDataChange({ ...formData, keys: next });
    };

    const handleRemoveKey = (idx: number) => {
        const curr = formData.keys ?? [];
        if (curr.length <= 1) return;
        const next = curr.filter((_, i) => i !== idx);
        onFormDataChange({ ...formData, keys: next });
    };

    const handleAddBaseUrl = () => {
        onFormDataChange({
            ...formData,
            base_urls: [...(formData.base_urls ?? []), { url: '', delay: 0 }],
        });
    };

    const handleUpdateBaseUrl = (idx: number, patch: Partial<Channel['base_urls'][number]>) => {
        const next = (formData.base_urls ?? []).map((u, i) => (i === idx ? { ...u, ...patch } : u));
        onFormDataChange({ ...formData, base_urls: next });
    };

    const handleRemoveBaseUrl = (idx: number) => {
        const curr = formData.base_urls ?? [];
        if (curr.length <= 1) return;
        onFormDataChange({ ...formData, base_urls: curr.filter((_, i) => i !== idx) });
    };

    const handleAddHeader = () => {
        onFormDataChange({
            ...formData,
            custom_header: [...(formData.custom_header ?? []), { header_key: '', header_value: '' }],
        });
    };

    const handleUpdateHeader = (idx: number, patch: Partial<Channel['custom_header'][number]>) => {
        const next = (formData.custom_header ?? []).map((h, i) => (i === idx ? { ...h, ...patch } : h));
        onFormDataChange({ ...formData, custom_header: next });
    };

    const handleRemoveHeader = (idx: number) => {
        const curr = formData.custom_header ?? [];
        if (curr.length <= 1) return;
        onFormDataChange({ ...formData, custom_header: curr.filter((_, i) => i !== idx) });
    };

    return (
        <form onSubmit={onSubmit} className="space-y-4 px-1">
            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-name`} className="text-sm font-medium text-card-foreground">
                        {t('name')}
                    </label>
                    <Input
                        className='rounded-xl'
                        id={`${idPrefix}-name`}
                        type="text"
                        value={formData.name}
                        onChange={(event) => onFormDataChange({ ...formData, name: event.target.value })}
                        required
                    />
                </div>

                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-type`} className="text-sm font-medium text-card-foreground">
                        {t('type')}
                    </label>
                    <Select
                        value={String(formData.type)}
                        onValueChange={(value) => onFormDataChange({ ...formData, type: value as ChannelType })}
                    >
                        <SelectTrigger id={`${idPrefix}-type`} className="rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                            <SelectValue />
                        </SelectTrigger>
                        <SelectContent className='rounded-xl'>
                            <SelectItem className='rounded-xl' value={String(ChannelType.OpenAIChat)}>{t('typeOpenAIChat')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.OpenAIResponse)}>{t('typeOpenAIResponse')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.Anthropic)}>{t('typeAnthropic')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.Gemini)}>{t('typeGemini')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.Volcengine)}>{t('typeVolcengine')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.OpenAIEmbedding)}>{t('typeOpenAIEmbedding')}</SelectItem>
                        </SelectContent>
                    </Select>
                </div>
            </div>

            <div className="space-y-2">
                <div className="flex items-center justify-between">
                    <label className="text-sm font-medium text-card-foreground">
                        {t('baseUrls')} {formData.base_urls.length > 0 ? `(${formData.base_urls.length})` : ''}
                    </label>
                    <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={handleAddBaseUrl}
                        className="h-6 px-2 text-xs text-muted-foreground/70 hover:text-muted-foreground hover:bg-transparent"
                    >
                        <Plus className="h-3 w-3 mr-1" />
                        {t('add')}
                    </Button>
                </div>
                <div className="space-y-2">
                    {(formData.base_urls ?? []).map((u, idx) => (
                        <div key={`baseurl-${idx}`} className="flex items-center gap-2">
                            <Input
                                id={`${idPrefix}-base-${idx}`}
                                type="url"
                                value={u.url}
                                onChange={(e) => handleUpdateBaseUrl(idx, { url: e.target.value })}
                                placeholder={t('baseUrlUrl')}
                                required={idx === 0}
                                className="rounded-xl flex-1"
                            />
                            <Button
                                type="button"
                                variant="ghost"
                                size="sm"
                                onClick={() => handleRemoveBaseUrl(idx)}
                                disabled={(formData.base_urls ?? []).length <= 1}
                                className="h-8 w-8 p-0 rounded-xl text-muted-foreground hover:text-destructive disabled:opacity-40 hover:bg-transparent"
                                title="Remove"
                            >
                                <X className="h-4 w-4" />
                            </Button>
                        </div>
                    ))}
                </div>
            </div>

            <div className="space-y-2">
                <div className="flex items-center justify-between">
                    <label className="text-sm font-medium text-card-foreground">
                        {t('apiKey')} {formData.keys.length > 0 ? `(${formData.keys.length})` : ''}
                    </label>
                    <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={handleAddKey}
                        className="h-6 px-2 text-xs text-muted-foreground/70 hover:text-muted-foreground hover:bg-transparent"
                    >
                        <Plus className="h-3 w-3 mr-1" />
                        {t('add')}
                    </Button>
                </div>
                <div className="space-y-2">
                    {(formData.keys ?? []).map((k, idx) => (
                        <div key={k.id ?? `new-${idx}`} className="flex items-center gap-2">
                            <Input
                                type="text"
                                value={k.channel_key}
                                onChange={(e) => handleUpdateKey(idx, { channel_key: e.target.value })}
                                placeholder={t('apiKey')}
                                required={idx === 0}
                                className="rounded-xl flex-1"
                            />
                            <Input
                                type="text"
                                value={k.remark ?? ''}
                                onChange={(e) => handleUpdateKey(idx, { remark: e.target.value })}
                                placeholder={t('remark')}
                                className="rounded-xl w-32"
                            />
                            <Input
                                type="text"
                                value={k.key_proxy ?? ''}
                                onChange={(e) => handleUpdateKey(idx, { key_proxy: e.target.value })}
                                placeholder={t('keyProxyPlaceholder')}
                                className="rounded-xl w-40"
                            />
                            <Switch
                                checked={k.enabled}
                                onCheckedChange={(checked) => handleUpdateKey(idx, { enabled: checked })}
                            />
                            <Button
                                type="button"
                                variant="ghost"
                                size="sm"
                                onClick={() => handleRemoveKey(idx)}
                                disabled={(formData.keys ?? []).length <= 1}
                                className="h-8 w-8 p-0 rounded-xl text-muted-foreground hover:text-destructive hover:bg-transparent disabled:opacity-40"
                                title="Remove"
                            >
                                <X className="h-4 w-4" />
                            </Button>
                        </div>
                    ))}
                </div>
            </div>

            <div className="space-y-2">
                <div className="flex items-center justify-between">
                    <label className="text-sm font-medium text-card-foreground">{t('model')}</label>
                    <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={handleRefreshModels}
                        disabled={!formData.base_urls?.[0]?.url || !effectiveKey || fetchModel.isPending}
                        className="h-6 px-2 text-xs text-muted-foreground/50 hover:text-muted-foreground hover:bg-transparent"
                    >
                        <RefreshCw className={`h-3 w-3 mr-1 ${fetchModel.isPending ? 'animate-spin' : ''}`} />
                        {t('modelRefresh')}
                    </Button>
                </div>
                <input type="hidden" value={formData.model} required />

                <div className="relative">
                    <Input
                        ref={inputRef}
                        id={`${idPrefix}-model-custom`}
                        type="text"
                        value={inputValue}
                        onChange={(e) => setInputValue(e.target.value)}
                        onKeyDown={handleInputKeyDown}
                        placeholder={t('modelCustomPlaceholder')}
                        className="pr-10 rounded-xl"
                    />
                    {inputValue.trim() && !selectedModelSet.has(inputValue.trim()) && (
                        <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={() => handleAddModel(inputValue)}
                            className="absolute rounded-lg right-1 top-1/2 -translate-y-1/2 h-7 w-7 p-0 text-muted-foreground hover:bg-accent hover:text-accent-foreground transition-colors"
                            title={t('modelAdd')}
                        >
                            <Plus className="size-4" />
                        </Button>
                    )}
                </div>

                <div className="space-y-2">
                    <Input
                        id={`${idPrefix}-model-search`}
                        type="search"
                        value={modelSearch}
                        onChange={(e) => setModelSearch(e.target.value)}
                        onKeyDown={handleModelSearchKeyDown}
                        placeholder={t('modelSearchPlaceholder')}
                        className="rounded-xl"
                    />
                    <div className="flex flex-wrap items-center justify-between gap-2">
                        <label className="text-xs font-medium text-card-foreground">
                            {t('modelSelected')} {selectedModels.length > 0 && `(${selectedModels.length})`}
                        </label>
                        <div className="flex items-center gap-1">
                            <Button
                                type="button"
                                variant="ghost"
                                size="sm"
                                onClick={() => selectModels(visibleModels)}
                                disabled={visibleModels.length === 0 || allVisibleModelsSelected}
                                className="h-6 px-2 text-xs text-muted-foreground/70 hover:text-muted-foreground hover:bg-transparent disabled:opacity-40"
                            >
                                {t('modelSelectAll')}
                            </Button>
                            <Button
                                type="button"
                                variant="ghost"
                                size="sm"
                                onClick={() => deselectModels(visibleModels)}
                                disabled={selectedVisibleModelCount === 0}
                                className="h-6 px-2 text-xs text-muted-foreground/70 hover:text-muted-foreground hover:bg-transparent disabled:opacity-40"
                            >
                                {t('modelDeselectAll')}
                            </Button>
                        </div>
                    </div>
                    <div className="rounded-xl border border-border bg-muted/30 p-2.5 max-h-40 min-h-12 overflow-y-auto">
                        {displayModels.length > 0 ? (
                            <div className="flex flex-wrap gap-1.5">
                                {displayModels.map((model) => {
                                    const isSelected = selectedModelSet.has(model);

                                    return (
                                        <button
                                            key={model}
                                            type="button"
                                            onClick={() => toggleModel(model)}
                                            className={`inline-flex items-center justify-center rounded-full border px-2 py-0.5 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${isSelected
                                                ? 'border-transparent bg-green-600 text-white hover:bg-green-700'
                                                : 'border-border bg-muted text-muted-foreground hover:bg-muted/80 hover:text-foreground'
                                                }`}
                                        >
                                            {model}
                                        </button>
                                    );
                                })}
                            </div>
                        ) : (
                            <div className="flex items-center justify-center h-8 text-xs text-muted-foreground">
                                {modelSearch.trim() ? t('modelNoMatches') : t('modelNoSelected')}
                            </div>
                        )}
                    </div>
                </div>
            </div>

            <Accordion type="single" collapsible className="w-full border rounded-xl bg-card">
                <AccordionItem value="advanced" className="border-none">
                    <AccordionTrigger className="text-sm font-medium text-card-foreground py-3 px-4 hover:no-underline hover:bg-muted/30 rounded-xl transition-colors">
                        {t('advanced')}
                    </AccordionTrigger>
                    <AccordionContent className="pt-4 px-4 pb-4 space-y-4 border-t">
                        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                            <div className="space-y-2">
                                <label htmlFor={`${idPrefix}-auto-group`} className="text-sm font-medium text-card-foreground">
                                    {t('autoGroup')}
                                </label>
                                <Select
                                    value={String(formData.auto_group)}
                                    onValueChange={(value) => onFormDataChange({ ...formData, auto_group: Number(value) as AutoGroupType })}
                                >
                                    <SelectTrigger id={`${idPrefix}-auto-group`} className="rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                                        <SelectValue />
                                    </SelectTrigger>
                                    <SelectContent className='rounded-xl'>
                                        <SelectItem className='rounded-xl' value={String(AutoGroupType.None)}>{t('autoGroupNone')}</SelectItem>
                                        <SelectItem className='rounded-xl' value={String(AutoGroupType.Fuzzy)}>{t('autoGroupFuzzy')}</SelectItem>
                                        <SelectItem className='rounded-xl' value={String(AutoGroupType.Exact)}>{t('autoGroupExact')}</SelectItem>
                                        <SelectItem className='rounded-xl' value={String(AutoGroupType.Regex)}>{t('autoGroupRegex')}</SelectItem>
                                    </SelectContent>
                                </Select>
                            </div>

                            <div className="space-y-2">
                                <label htmlFor={`${idPrefix}-channel-proxy`} className="text-sm font-medium text-card-foreground">
                                    {t('channelProxy')}
                                </label>
                                <Input
                                    id={`${idPrefix}-channel-proxy`}
                                    type="text"
                                    value={formData.channel_proxy}
                                    onChange={(e) => onFormDataChange({ ...formData, channel_proxy: e.target.value })}
                                    placeholder={t('channelProxyPlaceholder')}
                                    className="rounded-xl"
                                />
                            </div>
                        </div>

                        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                            <div className="space-y-2">
                                <label htmlFor={`${idPrefix}-key-mode`} className="text-sm font-medium text-card-foreground">
                                    {t('keyMode')}
                                </label>
                                <Select
                                    value={String(formData.key_mode)}
                                    onValueChange={(value) => onFormDataChange({ ...formData, key_mode: Number(value) })}
                                >
                                    <SelectTrigger id={`${idPrefix}-key-mode`} className="rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                                        <SelectValue />
                                    </SelectTrigger>
                                    <SelectContent className='rounded-xl'>
                                        <SelectItem className='rounded-xl' value="0">{t('keyModeCost')}</SelectItem>
                                        <SelectItem className='rounded-xl' value="1">{t('keyModeRoundRobin')}</SelectItem>
                                    </SelectContent>
                                </Select>
                            </div>

                            <div className="space-y-2">
                                <label htmlFor={`${idPrefix}-rate-limit`} className="text-sm font-medium text-card-foreground">
                                    {t('rateLimit')}
                                </label>
                                <Input
                                    id={`${idPrefix}-rate-limit`}
                                    type="text"
                                    value={formData.rate_limit}
                                    onChange={(e) => onFormDataChange({ ...formData, rate_limit: e.target.value })}
                                    placeholder={t('rateLimitPlaceholder')}
                                    className="rounded-xl"
                                />
                            </div>
                        </div>

                        <div className="space-y-2">
                            <label htmlFor={`${idPrefix}-model-rate-limit`} className="text-sm font-medium text-card-foreground">
                                {t('modelRateLimit')}
                            </label>
                            <Input
                                id={`${idPrefix}-model-rate-limit`}
                                type="text"
                                value={formData.model_rate_limit}
                                onChange={(e) => onFormDataChange({ ...formData, model_rate_limit: e.target.value })}
                                placeholder={t('modelRateLimitPlaceholder')}
                                className="rounded-xl"
                            />
                        </div>
                        <div className="space-y-2">
                            <div className="flex items-center justify-between">
                                <label className="text-sm font-medium text-card-foreground">
                                    {t('customHeader')} {formData.custom_header.length > 0 ? `(${formData.custom_header.length})` : ''}
                                </label>
                                <Button
                                    type="button"
                                    variant="ghost"
                                    size="sm"
                                    onClick={handleAddHeader}
                                    className="h-6 px-2 text-xs text-muted-foreground/70 hover:text-muted-foreground hover:bg-transparent"
                                >
                                    <Plus className="h-3 w-3 mr-1" />
                                    {t('customHeaderAdd')}
                                </Button>
                            </div>
                            <div className="space-y-2">
                                {(formData.custom_header ?? []).map((h, idx) => (
                                    <div key={`hdr-${idx}`} className="flex items-center gap-2">
                                        <Input
                                            type="text"
                                            value={h.header_key}
                                            onChange={(e) => handleUpdateHeader(idx, { header_key: e.target.value })}
                                            placeholder={t('customHeaderKey')}
                                            className="rounded-xl flex-1"
                                        />
                                        <Input
                                            type="text"
                                            value={h.header_value}
                                            onChange={(e) => handleUpdateHeader(idx, { header_value: e.target.value })}
                                            placeholder={t('customHeaderValue')}
                                            className="rounded-xl flex-1"
                                        />
                                        <Button
                                            type="button"
                                            variant="ghost"
                                            size="sm"
                                            onClick={() => handleRemoveHeader(idx)}
                                            disabled={(formData.custom_header ?? []).length <= 1}
                                            className="h-8 w-8 p-0 rounded-xl text-muted-foreground hover:text-destructive hover:bg-transparent disabled:opacity-40"
                                            title="Remove"
                                        >
                                            <X className="h-4 w-4" />
                                        </Button>
                                    </div>
                                ))}
                            </div>
                        </div>

                        <div className="space-y-2">
                            <label htmlFor={`${idPrefix}-match-regex`} className="text-sm font-medium text-card-foreground">
                                {t('matchRegex')}
                            </label>
                            <Input
                                id={`${idPrefix}-match-regex`}
                                type="text"
                                value={formData.match_regex}
                                onChange={(e) => onFormDataChange({ ...formData, match_regex: e.target.value })}
                                placeholder={t('matchRegexPlaceholder')}
                                className="rounded-xl"
                            />
                        </div>

                        <div className="space-y-2">
                            <label htmlFor={`${idPrefix}-param-override`} className="text-sm font-medium text-card-foreground">
                                {t('paramOverride')}
                            </label>
                            <textarea
                                id={`${idPrefix}-param-override`}
                                value={formData.param_override}
                                onChange={(e) => onFormDataChange({ ...formData, param_override: e.target.value })}
                                placeholder={t('paramOverridePlaceholder')}
                                className="min-h-28 w-full rounded-xl border border-border bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                            />
                        </div>
                    </AccordionContent>
                </AccordionItem>
            </Accordion>

            <div className="flex flex-wrap items-center justify-between gap-4 p-4 rounded-xl bg-muted/20 border border-border/50">
                <label className="flex items-center gap-2 cursor-pointer">
                    <Switch
                        checked={formData.enabled}
                        onCheckedChange={(checked) => onFormDataChange({ ...formData, enabled: checked })}
                    />
                    <span className="text-sm font-medium text-card-foreground">{t('enabled')}</span>
                </label>
                <div className="flex items-center gap-6">
                    <label className="flex items-center gap-2 cursor-pointer">
                        <Switch
                            checked={formData.proxy}
                            onCheckedChange={(checked) => onFormDataChange({ ...formData, proxy: checked })}
                        />
                        <span className="text-sm text-card-foreground">{t('proxy')}</span>
                    </label>
                    <label className="flex items-center gap-2 cursor-pointer">
                        <Switch
                            checked={formData.auto_sync}
                            onCheckedChange={(checked) => onFormDataChange({ ...formData, auto_sync: checked })}
                        />
                        <span className="text-sm text-card-foreground">{t('autoSync')}</span>
                    </label>
                </div>
            </div>

            <div className={`flex flex-col gap-3 pt-2 ${onCancel ? 'sm:flex-row' : ''}`}>
                {onCancel && cancelText && (
                    <Button
                        type="button"
                        variant="secondary"
                        onClick={onCancel}
                        className="w-full sm:flex-1 rounded-2xl h-12"
                    >
                        {cancelText}
                    </Button>
                )}
                <Button
                    type="submit"
                    disabled={isPending}
                    className="w-full sm:flex-1 rounded-2xl h-12"
                >
                    {isPending ? pendingText : submitText}
                </Button>
            </div>
        </form>
    );
}
